package plugins

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/appconfig"
	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

// ErrCapsChanged is returned when an upgrade changes the capability set and
// the admin has not re-approved it.
var ErrCapsChanged = errors.New("plugins: capabilities changed, re-approval required")

// Installer performs verified plugin installs and removals.
type Installer struct {
	Host        *Host
	Registry    *Registry
	InstallDir  string
	TrustedKeys []ed25519.PublicKey
	Logger      *slog.Logger

	// Uninstall cleanup dependencies. Any nil one skips its cleanup step:
	// JobStore drops the plugin's queued plugin.<id> job rows, AppConfig
	// drops the plugin's "<id>.*" config rows, SystemStorage +
	// SystemPrefix remove the plugin's system-storage tree
	// (<SystemPrefix>/<id>), and Cache drops the plugin's "plugin:<id>:"
	// cache keys (ADR-0058).
	JobStore      jobs.Store
	AppConfig     *appconfig.Store
	SystemStorage storage.Storage
	SystemPrefix  string
	Cache         cache.Cache
}

// InstallOptions controls install-time policy.
type InstallOptions struct {
	// ForceUnsigned permits archives without signature.sig.
	ForceUnsigned bool
	// ApproveCaps re-approves capability changes on upgrade.
	ApproveCaps bool
}

// LoadTrustedKeys reads every *.pub file under dir (base64-encoded ed25519
// public keys, one per line). A missing directory yields no keys.
func LoadTrustedKeys(dir string) ([]ed25519.PublicKey, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("plugins: trusted keys: %w", err)
	}
	var keys []ed25519.PublicKey
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("plugins: trusted keys: %w", err)
		}
		line, _, _ := strings.Cut(string(raw), "\n")
		pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: %s: malformed public key", ErrSignatureInvalid, e.Name())
		}
		keys = append(keys, ed25519.PublicKey(pub))
	}
	return keys, nil
}

// TrustedKeysDir is the conventional key directory inside installDir.
func TrustedKeysDir(installDir string) string {
	return filepath.Join(installDir, "trusted_keys")
}

// capsSnapshot marshals the granted capability set for registry storage.
func capsSnapshot(c *Capabilities) json.RawMessage {
	raw, err := json.Marshal(c)
	if err != nil { // unreachable: only strings/slices/bools
		panic(err)
	}
	return raw
}

// capsChanged compares the stored snapshot against a new manifest's request.
// Unparseable snapshots count as changed, forcing re-approval.
func capsChanged(stored json.RawMessage, requested *Capabilities) bool {
	if len(stored) == 0 {
		stored = json.RawMessage(`{}`)
	}
	var storedCaps, requestedCaps Capabilities
	if err := json.Unmarshal(stored, &storedCaps); err != nil {
		return true
	}
	if requested != nil {
		requestedCaps = *requested
	}
	oldRaw, err := json.Marshal(storedCaps)
	if err != nil {
		return true
	}
	newRaw, err := json.Marshal(requestedCaps)
	if err != nil {
		return true
	}
	return string(oldRaw) != string(newRaw)
}

// Install verifies, stores, hooks, and registers a plugin archive.
func (in *Installer) Install(ctx context.Context, raw []byte, opts InstallOptions) (*RegistryRow, error) {
	a, err := ReadArchive(raw)
	if err != nil {
		return nil, err
	}

	keyID := ""
	if a.Signature == nil {
		if !opts.ForceUnsigned {
			return nil, ErrUnsigned
		}
	} else {
		keyID, err = VerifyMembers(a.Signature, a.Members(), in.TrustedKeys)
		if err != nil {
			return nil, err
		}
	}

	p, err := in.Host.Load(ctx, a.Manifest, a.Module)
	if err != nil {
		return nil, err
	}
	defer func() { _ = p.Close(ctx) }()

	id := a.Manifest.Plugin.ID
	existing, err := in.Registry.Get(ctx, id)
	upgrade := false
	fromVersion := ""
	switch {
	case err == nil:
		if !opts.ApproveCaps && capsChanged(existing.Capabilities, &a.Manifest.Capabilities) {
			return nil, fmt.Errorf("%w: %s", ErrCapsChanged, id)
		}
		upgrade = true
		fromVersion = existing.Version
	case errors.Is(err, ErrPluginNotFound):
	default:
		return nil, err
	}

	// Upgrade ordering (ADR-0056): after the signature/capability gates and
	// BEFORE storing the new archive, clear the previous version's persisted
	// route/prop registrations and run the upgrade hook. Upsert-only
	// registrations would otherwise linger when the new version no longer
	// declares them. on_upgrade receives the from-version string; without an
	// on_upgrade entry point the installer falls back to on_install so
	// route-registering plugins keep working. A failing hook leaves the old
	// archive installed (its registrations are re-created on the next
	// successful install).
	if upgrade {
		if err := in.Registry.DeleteRoutesForPlugin(ctx, id); err != nil {
			return nil, err
		}
		if err := in.Registry.DeletePropsForPlugin(ctx, id); err != nil {
			return nil, err
		}
		if err := p.Upgrade(ctx, fromVersion); err != nil {
			return nil, err
		}
	}

	dir := filepath.Join(in.InstallDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("plugins: install dir: %w", err)
	}
	archivePath := filepath.Join(dir, a.Manifest.Plugin.Version+".ncplugin")
	if err := os.WriteFile(archivePath, raw, 0o600); err != nil {
		return nil, fmt.Errorf("plugins: store archive: %w", err)
	}

	if !upgrade {
		if err := p.Install(ctx); err != nil {
			_ = os.Remove(archivePath)
			return nil, err
		}
	}

	row := &RegistryRow{
		ID:             id,
		Version:        a.Manifest.Plugin.Version,
		Enabled:        true,
		Capabilities:   capsSnapshot(&a.Manifest.Capabilities),
		SignatureKeyID: keyID,
		ArchivePath:    archivePath,
	}
	if err := in.Registry.Upsert(ctx, row); err != nil {
		_ = os.Remove(archivePath)
		return nil, err
	}
	return row, nil
}

// Uninstall runs the on_uninstall hook best-effort, then removes everything
// the plugin persisted: route and WebDAV prop records, queued job rows, its
// appconfig keys, its "plugin:<id>:" cache keys, its system-storage tree,
// the registry record, and the stored archive. Without the
// jobs/appconfig/cache/storage steps an uninstalled plugin left rows the
// jobs runner retried forever, state it could never reclaim, and (in a
// Redis-backed cache) stale keys a reinstalled plugin would read.
func (in *Installer) Uninstall(ctx context.Context, id string) error {
	row, err := in.Registry.Get(ctx, id)
	if err != nil {
		return err
	}
	if raw, rerr := os.ReadFile(row.ArchivePath); rerr == nil {
		if a, aerr := ReadArchive(raw); aerr == nil {
			if p, perr := in.Host.Load(ctx, a.Manifest, a.Module); perr == nil {
				if uerr := p.Uninstall(ctx); uerr != nil && in.Logger != nil {
					in.Logger.WarnContext(ctx, "plugins: uninstall hook failed",
						slog.String("plugin.id", id), slog.String("error", uerr.Error()))
				}
				if cerr := p.Close(ctx); cerr != nil && in.Logger != nil {
					in.Logger.WarnContext(ctx, "plugins: close failed",
						slog.String("plugin.id", id), slog.String("error", cerr.Error()))
				}
			}
		}
	}
	if err := in.Registry.DeleteRoutesForPlugin(ctx, id); err != nil {
		return err
	}
	if err := in.Registry.DeletePropsForPlugin(ctx, id); err != nil {
		return err
	}
	if in.JobStore != nil {
		if err := in.JobStore.DeleteByName(ctx, pluginJobName(id)); err != nil {
			return err
		}
	}
	if in.AppConfig != nil {
		if err := in.AppConfig.DeleteByPrefix(ctx, configAppID, id+"."); err != nil {
			return err
		}
	}
	if in.Cache != nil {
		n, err := in.Cache.DeleteByPrefix(ctx, cacheKeyPrefix(id))
		if err != nil {
			return err
		}
		if n > 0 && in.Logger != nil {
			in.Logger.InfoContext(ctx, "plugins: uninstall purged cache keys",
				slog.String("plugin.id", id), slog.Int64("cache.deleted", n))
		}
	}
	if in.SystemStorage != nil && in.SystemPrefix != "" {
		if err := deleteTree(ctx, in.SystemStorage, in.SystemPrefix+"/"+id); err != nil {
			return err
		}
	}
	if err := in.Registry.Delete(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(in.InstallDir, id))
}
