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
	switch {
	case err == nil:
		if !opts.ApproveCaps && capsChanged(existing.Capabilities, &a.Manifest.Capabilities) {
			return nil, fmt.Errorf("%w: %s", ErrCapsChanged, id)
		}
	case errors.Is(err, ErrPluginNotFound):
	default:
		return nil, err
	}

	dir := filepath.Join(in.InstallDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("plugins: install dir: %w", err)
	}
	archivePath := filepath.Join(dir, a.Manifest.Plugin.Version+".ncplugin")
	if err := os.WriteFile(archivePath, raw, 0o600); err != nil {
		return nil, fmt.Errorf("plugins: store archive: %w", err)
	}

	if err := p.Install(ctx); err != nil {
		_ = os.Remove(archivePath)
		return nil, err
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

// Uninstall runs the on_uninstall hook best-effort, then removes the
// registry record and the stored archive.
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
	if err := in.Registry.Delete(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(in.InstallDir, id))
}
