package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

func newEncryption() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "encryption",
		Short: "Server-side encryption at rest (ADR-0052)",
		Long: "Manage the master key for transparent server-side encryption at rest.\n\n" +
			"Enable it in the server config:\n\n" +
			"  encryption:\n" +
			"    enabled: true\n" +
			"    master_key_path: /var/lib/ncgo/master.key\n\n" +
			"Files written while encryption is on are sealed (AES-256-GCM);\n" +
			"pre-existing plaintext files keep working and are sealed when\n" +
			"rewritten.",
	}
	cmd.AddCommand(newEncryptionInit(), newEncryptionStatus(), newEncryptionEncryptAll(), newEncryptionDecryptAll())
	return cmd
}

func newEncryptionEncryptAll() *cobra.Command {
	return newEncryptionSweep(encrypt.SweepSeal)
}

func newEncryptionDecryptAll() *cobra.Command {
	return newEncryptionSweep(encrypt.SweepOpen)
}

// newEncryptionSweep builds `encryption encrypt-all|decrypt-all`: an
// in-place re-encoding sweep over the whole storage tree (or one user's
// subtree), sealing legacy plaintext files or writing sealed files back as
// plaintext for decommissioning.
func newEncryptionSweep(direction encrypt.SweepDirection) *cobra.Command {
	var user string
	var dryRun bool
	verb := "encrypt-all"
	action := "seal"
	past := "sealed"
	if direction == encrypt.SweepOpen {
		verb = "decrypt-all"
		action = "restore"
		past = "decrypted"
	}
	cmd := &cobra.Command{
		Use:   verb,
		Short: "Re-encode stored files in place (" + direction.String() + " the whole tree)",
		Long: "Walk the default storage backend and " + action + " every file that is not\n" +
			"already in the target encoding, in place. With --user the sweep is limited\n" +
			"to that user's subtree (<uid>/); otherwise the entire backend is covered.\n\n" +
			"The sweep is idempotent (interrupted runs can simply be re-run) and safe to\n" +
			"run against a live server: every write replaces the file atomically, and\n" +
			"the server's encryption layer auto-detects the encoding, so concurrent\n" +
			"reads always see correct content. Running it at low-traffic times is still\n" +
			"recommended: a file rewritten by a user concurrently with the sweep could\n" +
			"lose that write. File metadata (filecache, versions, etags) is untouched —\n" +
			"the plaintext content does not change, only its encoding at rest.\n\n" +
			"Requires encryption.enabled and a loadable master key; decrypt-all aborts\n" +
			"at the first file whose key does not match.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if !cfg.Encryption.Enabled {
				return fmt.Errorf("ncgo-cli: encryption.enabled is false; enable encryption before running %s", verb)
			}
			user = strings.TrimSpace(user)
			if strings.ContainsAny(user, `/\`) || user == "." || user == ".." {
				return fmt.Errorf("ncgo-cli: invalid --user %q", user)
			}
			key, err := encrypt.LoadMasterKey(cfg.Encryption.MasterKeyPath)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption: %w", err)
			}
			raw, err := openRawBackend(cfg)
			if err != nil {
				return err
			}
			enc, err := encrypt.New(key, raw)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption: %w", err)
			}
			prefix := ""
			if user != "" {
				prefix = user + "/"
			}
			out := cmd.OutOrStdout()
			opts := encrypt.SweepOptions{
				Direction: direction,
				Prefix:    prefix,
				DryRun:    dryRun,
				OnError: func(path string, err error) {
					fmt.Fprintf(out, "failed: %s: %v\n", path, err)
				},
			}
			const progressEvery = 100
			if !dryRun {
				opts.Progress = func(done, total int64) {
					if done%progressEvery == 0 || done == total {
						fmt.Fprintf(out, "%s: %d/%d files\n", verb, done, total)
					}
				}
			}
			stats, err := encrypt.Sweep(cmd.Context(), raw, enc, opts)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption %s: %w", verb, err)
			}
			if dryRun {
				fmt.Fprintf(out, "dry-run %s: scanned=%d changed=%d skipped=%d failed=%d bytes=%d\n",
					verb, stats.Scanned, stats.Changed, stats.Skipped, stats.Failed, stats.Bytes)
			} else {
				fmt.Fprintf(out, "%s: scanned=%d %s=%d skipped=%d failed=%d bytes=%d\n",
					verb, stats.Scanned, past, stats.Changed, stats.Skipped, stats.Failed, stats.Bytes)
			}
			if stats.Failed > 0 {
				return fmt.Errorf("ncgo-cli: encryption %s: %d file(s) failed", verb, stats.Failed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "limit the sweep to one user's tree (the <uid>/ storage prefix)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count what would change without writing anything")
	return cmd
}

func newEncryptionInit() *cobra.Command {
	var keyPath string
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a new master key file (mode 0600)",
		Long: "Generate 32 random bytes and write them base64-encoded to the key file.\n\n" +
			"The key path comes from --key-path or encryption.master_key_path in the\n" +
			"config. An existing key file is never overwritten without --force.\n" +
			"The key itself is never printed; back up the key file securely.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if strings.TrimSpace(keyPath) == "" {
				keyPath = cfg.Encryption.MasterKeyPath
			}
			if strings.TrimSpace(keyPath) == "" {
				return fmt.Errorf("ncgo-cli: no key path: pass --key-path or set encryption.master_key_path")
			}
			key := make([]byte, encrypt.MasterKeySize)
			if _, err := rand.Read(key); err != nil {
				return fmt.Errorf("ncgo-cli: generate key: %w", err)
			}
			flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
			if force {
				flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			}
			fh, err := os.OpenFile(keyPath, flags, 0o600)
			if err != nil {
				if errors.Is(err, fs.ErrExist) {
					return fmt.Errorf("ncgo-cli: key file %s already exists (use --force to overwrite)", keyPath)
				}
				return fmt.Errorf("ncgo-cli: create key file: %w", err)
			}
			_, werr := fh.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
			cerr := fh.Close()
			if err := errors.Join(werr, cerr); err != nil {
				return fmt.Errorf("ncgo-cli: write key file: %w", err)
			}
			// Enforce 0600 even when --force reused a pre-existing file.
			if err := os.Chmod(keyPath, 0o600); err != nil {
				return fmt.Errorf("ncgo-cli: chmod key file: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote master key to %s (mode 0600)\n", keyPath)
			if !cfg.Encryption.Enabled {
				fmt.Fprintln(cmd.OutOrStdout(), "encryption is disabled; set encryption.enabled: true and encryption.master_key_path in the config to activate")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&keyPath, "key-path", "", "path for the key file (default: encryption.master_key_path from config)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing key file")
	return cmd
}

func newEncryptionStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show encryption state and key file health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "encryption.enabled: %v\n", cfg.Encryption.Enabled)
			if strings.TrimSpace(cfg.Encryption.MasterKeyPath) == "" {
				fmt.Fprintln(out, "master key path: (not set)")
				if cfg.Encryption.Enabled {
					return fmt.Errorf("ncgo-cli: encryption enabled but encryption.master_key_path is not set")
				}
				return nil
			}
			fmt.Fprintf(out, "master key path: %s\n", cfg.Encryption.MasterKeyPath)
			info, statErr := os.Stat(cfg.Encryption.MasterKeyPath)
			switch {
			case errors.Is(statErr, fs.ErrNotExist):
				fmt.Fprintln(out, "master key: MISSING")
				return fmt.Errorf("ncgo-cli: key file %s does not exist (run: ncgo-cli encryption init)", cfg.Encryption.MasterKeyPath)
			case statErr != nil:
				return fmt.Errorf("ncgo-cli: stat key file: %w", statErr)
			}
			if _, err := encrypt.LoadMasterKey(cfg.Encryption.MasterKeyPath); err != nil {
				fmt.Fprintf(out, "master key: UNUSABLE (%v)\n", err)
				return fmt.Errorf("ncgo-cli: master key does not load: %w", err)
			}
			fmt.Fprintf(out, "master key: OK (32 bytes, mode %04o)\n", info.Mode().Perm())
			return nil
		},
	}
}
