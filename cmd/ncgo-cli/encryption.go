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
	cmd.AddCommand(newEncryptionInit(), newEncryptionStatus())
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
