package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/appconfig"
	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

func newPlugin() *cobra.Command {
	cmd := &cobra.Command{Use: "plugin", Short: "Plugin tools"}
	cmd.AddCommand(
		newPluginCheck(),
		newPluginKeygen(),
		newPluginPack(),
		newPluginSign(),
		newPluginVerify(),
		newPluginInstall(),
		newPluginUninstall(),
		newPluginList(),
		newPluginEnable("enable <plugin-id>", true),
		newPluginEnable("disable <plugin-id>", false),
	)
	return cmd
}

func newPluginCheck() *cobra.Command {
	return &cobra.Command{
		Use:   "check <dir>",
		Short: "Load and install a plugin from a directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			f, err := os.Open(filepath.Join(dir, "plugin.toml"))
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			m, err := plugins.ParseManifest(f)
			if err != nil {
				return err
			}
			wasm, err := os.ReadFile(filepath.Join(dir, m.EntryPoints.Module))
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			h, err := plugins.NewHost(ctx, plugins.HostConfig{}, slog.New(slog.DiscardHandler))
			if err != nil {
				return err
			}
			defer func() { _ = h.Close(ctx) }()
			p, err := h.Load(ctx, m, wasm)
			if err != nil {
				return err
			}
			defer func() { _ = p.Close(ctx) }()
			if err := p.Install(ctx); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok %s %s\n", m.Plugin.ID, m.Plugin.Version)
			return nil
		},
	}
}

func newPluginKeygen() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate an ed25519 signing key pair (<out>.key / <out>.pub)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return errors.New("--out is required")
			}
			pub, priv, err := plugins.GenerateKey()
			if err != nil {
				return err
			}
			if err := writeKeyFile(out+".key", priv, 0o600); err != nil {
				return err
			}
			if err := writeKeyFile(out+".pub", pub, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "keyid %s\n", plugins.KeyIDFromPublic(pub))
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "output path prefix")
	return cmd
}

func writeKeyFile(path string, key []byte, mode fs.FileMode) error {
	enc := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(path, []byte(enc+"\n"), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s: malformed private key", path)
	}
	return ed25519.PrivateKey(priv), nil
}

func newPluginPack() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "pack <dir>",
		Short: "Pack a plugin directory into a .ncplugin archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return errors.New("--out is required")
			}
			members, err := packDir(args[0])
			if err != nil {
				return err
			}
			raw, err := plugins.WriteArchive(members)
			if err != nil {
				return err
			}
			if _, err := plugins.ReadArchive(raw); err != nil {
				return fmt.Errorf("packed archive fails validation: %w", err)
			}
			if err := os.WriteFile(out, raw, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "packed %s (%d bytes)\n", out, len(raw))
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "output .ncplugin path")
	return cmd
}

// packDir collects plugin files: plugin.toml, the wasm module, and any extra
// regular files (i18n/, README, settings schema). Keys, signatures, and
// hidden files are excluded.
func packDir(dir string) (map[string][]byte, error) {
	members := make(map[string][]byte)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".key") ||
			strings.HasSuffix(base, ".pub") || base == plugins.SignatureFile {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G122: operator-supplied local dir in a CLI dev tool
		if err != nil {
			return err
		}
		members[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := members["plugin.toml"]; !ok {
		return nil, errors.New("plugin.toml not found")
	}
	return members, nil
}

func newPluginSign() *cobra.Command {
	var keyPath, out string
	cmd := &cobra.Command{
		Use:   "sign <archive.ncplugin>",
		Short: "Sign an archive with an ed25519 private key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			priv, err := readPrivateKey(keyPath)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			a, err := plugins.ReadArchive(raw)
			if err != nil {
				return err
			}
			sigRaw, err := plugins.SignMembers(a.Members(), priv)
			if err != nil {
				return err
			}
			members := a.Members()
			members[plugins.SignatureFile] = sigRaw
			packed, err := plugins.WriteArchive(members)
			if err != nil {
				return err
			}
			if out == "" {
				out = args[0]
			}
			if err := os.WriteFile(out, packed, 0o600); err != nil { //nolint:gosec // G703: output path is an operator-supplied CLI flag
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "signed %s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyPath, "key", "", "private key file")
	cmd.Flags().StringVarP(&out, "out", "o", "", "output path (default: in place)")
	return cmd
}

func newPluginVerify() *cobra.Command {
	var trustedDir string
	cmd := &cobra.Command{
		Use:   "verify <archive.ncplugin>",
		Short: "Verify an archive's signature against trusted keys",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if trustedDir == "" {
				return errors.New("--trusted-dir is required")
			}
			keys, err := plugins.LoadTrustedKeys(trustedDir)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			a, err := plugins.ReadArchive(raw)
			if err != nil {
				return err
			}
			if a.Signature == nil {
				return plugins.ErrUnsigned
			}
			keyID, err := plugins.VerifyMembers(a.Signature, a.Members(), keys)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok %s %s keyid %s\n", a.Manifest.Plugin.ID, a.Manifest.Plugin.Version, keyID)
			return nil
		},
	}
	cmd.Flags().StringVar(&trustedDir, "trusted-dir", "", "directory of trusted .pub keys")
	return cmd
}

// installHostConfig assembles the full install-time plugin host
// configuration: every subsystem the server wires in app.New, so lifecycle
// hooks (on_install/on_upgrade/on_uninstall) can use every capability —
// route/OCS/WebDAV-prop registration, db_*, config_*, job_enqueue,
// storage_*, event_publish. Cache is deliberately nil: the CLI is a
// management surface and cache is runtime state, so cache_* hooks get -12
// (ADR-0056). Metrics stays nil (no listener in the CLI). The jobs runner
// is constructed but never started — Enqueue only inserts rows, which the
// server picks up on next boot.
func installHostConfig(cfg *config.Config, db database.DB, st storage.Storage) plugins.HostConfig {
	return plugins.HostConfig{
		DB:                   db,
		Bus:                  events.NewBus(slog.New(slog.DiscardHandler)),
		Registry:             plugins.NewRegistry(db),
		Files:                filesDAV(st, db),
		SystemStorage:        st,
		SystemPrefix:         "appdata_" + cliInstanceID(cfg) + "/plugins",
		AppConfig:            appconfig.NewStore(db),
		Jobs:                 jobs.NewRunner(jobs.NewSQLStore(db), nil, 1, time.Minute),
		HTTPRatePerMinute:    cfg.Plugin.HTTPRatePerMinute,
		MaxHTTPResponseBytes: int64(cfg.Plugin.MaxHTTPResponseMB) << 20,
		// Hooks run DDL/DML through db_*, so the quota applies here too.
		DBMaxConcurrentPerPlugin: cfg.Plugin.DBMaxConcurrentPerPlugin,
		// Hooks can write system storage (storage_*), so the tree quota
		// applies here too.
		PluginSystemQuotaBytes: int64(cfg.Plugin.SystemStorageQuotaMB) << 20,
	}
}

// newInstallHost builds the install-time plugin host from installHostConfig.
func newInstallHost(ctx context.Context, cfg *config.Config, db database.DB, st storage.Storage) (*plugins.Host, error) {
	return plugins.NewHost(ctx, installHostConfig(cfg, db, st), slog.New(slog.DiscardHandler))
}

// pluginInstaller wires config, DB, storage, host, and trusted keys for
// install-time commands.
func pluginInstaller(cmd *cobra.Command) (*plugins.Installer, func(), error) {
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		return nil, nil, err
	}
	ctx := cmd.Context()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	st, err := openStorage(cfg)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	h, err := newInstallHost(ctx, cfg, db, st)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	keys, err := plugins.LoadTrustedKeys(plugins.TrustedKeysDir(cfg.Plugin.InstallDir))
	if err != nil {
		_ = h.Close(ctx)
		_ = db.Close()
		return nil, nil, err
	}
	in := &plugins.Installer{
		Host:          h,
		Registry:      plugins.NewRegistry(db),
		InstallDir:    cfg.Plugin.InstallDir,
		TrustedKeys:   keys,
		Logger:        slog.New(slog.DiscardHandler),
		JobStore:      jobs.NewSQLStore(db),
		AppConfig:     appconfig.NewStore(db),
		SystemStorage: st,
		SystemPrefix:  "appdata_" + cliInstanceID(cfg) + "/plugins",
	}
	// Only a Redis-backed deployment gets cache cleanup from the CLI: the
	// shared L2 survives server restarts, so a reinstalled plugin would see
	// the previous generation's keys. A memory-only cache lives inside the
	// server process — a CLI-local Memory would be an empty shell, and the
	// server's copy dies with the process. (With hot reload, ADR-0062, an
	// uninstall no longer implies a restart, so a memory-cache reinstall
	// within one process lifetime can see stale keys — a reconciler-side
	// purge is a documented follow-up.) The install host's Cache stays
	// deliberately nil (ADR-0056 G1).
	var rc *cache.Redis
	if cfg.Cache.RedisAddr != "" {
		rc, err = cache.NewRedis(cache.RedisConfig{
			Addr:     cfg.Cache.RedisAddr,
			Password: cfg.Cache.RedisPassword,
			DB:       cfg.Cache.RedisDB,
		})
		if err != nil {
			_ = h.Close(ctx)
			_ = db.Close()
			return nil, nil, err
		}
		in.Cache = rc
	}
	cleanup := func() {
		if rc != nil {
			_ = rc.Close()
		}
		_ = h.Close(ctx)
		_ = db.Close()
	}
	return in, cleanup, nil
}

func newPluginInstall() *cobra.Command {
	var forceUnsigned, approveCaps bool
	cmd := &cobra.Command{
		Use:   "install <archive.ncplugin>",
		Short: "Verify and install a plugin archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in, cleanup, err := pluginInstaller(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			row, err := in.Install(cmd.Context(), raw, plugins.InstallOptions{
				ForceUnsigned: forceUnsigned,
				ApproveCaps:   approveCaps,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed %s %s\n", row.ID, row.Version)
			return nil
		},
	}
	cmd.Flags().BoolVar(&forceUnsigned, "force-unsigned", false, "install archives without a signature")
	cmd.Flags().BoolVar(&approveCaps, "approve-caps", false, "re-approve capability changes on upgrade")
	return cmd
}

func newPluginUninstall() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall <plugin-id>",
		Short: "Remove an installed plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in, cleanup, err := pluginInstaller(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := in.Uninstall(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "uninstalled %s\n", args[0])
			return nil
		},
	}
}

func newPluginList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed plugins",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := database.Open(ctx, database.Config{
				Driver: database.Dialect(cfg.Database.Driver),
				DSN:    cfg.Database.DSN,
			})
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			rows, err := plugins.NewRegistry(db).List(ctx)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tVERSION\tENABLED\tKEYID")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%t\t%s\n", r.ID, r.Version, r.Enabled, r.SignatureKeyID)
			}
			return tw.Flush()
		},
	}
}

func newPluginEnable(use string, enabled bool) *cobra.Command {
	verb := strings.Split(use, " ")[0]
	return &cobra.Command{
		Use:   use,
		Short: verb + " an installed plugin (takes effect within plugin.refresh_interval)",
		Long: verb + " an installed plugin by flipping its enabled flag in the registry.\n\n" +
			"The running server's plugin reconciler polls the registry, so the change\n" +
			"takes effect within plugin.refresh_interval (default 10s; 0 disables the\n" +
			"poll, in which case a SERVER RESTART is required).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := database.Open(ctx, database.Config{
				Driver: database.Dialect(cfg.Database.Driver),
				DSN:    cfg.Database.DSN,
			})
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := plugins.NewRegistry(db).SetEnabled(ctx, args[0], enabled); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%sd %s\n", verb, args[0])
			return nil
		},
	}
}
