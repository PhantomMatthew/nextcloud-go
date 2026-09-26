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

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
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
			"rewritten.\n\n" +
			"Master-key rotation (ADR-0074):\n" +
			"  1. ncgo-cli encryption init --key-path <new-key-file>\n" +
			"  2. Move the OLD master_key_path into encryption.previous_key_paths\n" +
			"     (APPEND at the end when the list is non-empty) and set\n" +
			"     master_key_path to the new key.\n" +
			"  3. Restart the server — old files keep reading, new writes seal\n" +
			"     under the new (highest-ID) key.\n" +
			"  4. ncgo-cli encryption rotate-keys\n" +
			"  5. previous_key_paths is APPEND-ONLY FOREVER — never reorder or\n" +
			"     remove entries: key IDs are positional, and removing or\n" +
			"     reordering orphans files with \"unknown key id\" read errors.\n\n" +
			"Per-user keys migration (ADR-0097):\n" +
			"  1. Set encryption.per_user_keys: true and restart — new writes seal\n" +
			"     with the v3 per-user-key envelope; existing v1/v2/plaintext files\n" +
			"     keep reading.\n" +
			"  2. ncgo-cli encryption encrypt-all to seal any legacy plaintext\n" +
			"     (lands on v3 directly in per-user mode).\n" +
			"  3. ncgo-cli encryption rekey-v3 to re-seal every v1/v2 file into\n" +
			"     the v3 envelope. Idempotent; safe to re-run after an\n" +
			"     interruption. Once v3 files exist, rolling back to a pre-v3\n" +
			"     binary strands them (same rule as v2).\n" +
			"  4. ncgo-cli encryption reconcile — mints user keys for pre-existing\n" +
			"     users, wraps existing shares' file keys for their recipients,\n" +
			"     and prunes stale key rows.\n\n" +
			"rotate-keys also re-seals user keys under the current key id\n" +
			"(ADR-0099), so retired master keys can eventually leave the ring.",
	}
	cmd.AddCommand(newEncryptionInit(), newEncryptionStatus(), newEncryptionEncryptAll(), newEncryptionDecryptAll(), newEncryptionRotateKeys(), newEncryptionRekeyV3(), newEncryptionReconcile())
	return cmd
}

func newEncryptionEncryptAll() *cobra.Command {
	return newEncryptionSweep(encrypt.SweepSeal)
}

func newEncryptionDecryptAll() *cobra.Command {
	return newEncryptionSweep(encrypt.SweepOpen)
}

func newEncryptionRotateKeys() *cobra.Command {
	return newEncryptionSweep(encrypt.SweepRotate)
}

func newEncryptionRekeyV3() *cobra.Command {
	return newEncryptionSweep(encrypt.SweepRekeyV3)
}

// newEncryptionSweep builds `encryption encrypt-all|decrypt-all|rotate-keys|rekey-v3`:
// an in-place re-encoding sweep over the whole storage tree (or one user's
// subtree), sealing legacy plaintext files, writing sealed files back as
// plaintext for decommissioning, re-sealing retired-key files under the
// keyring's current key, or re-sealing v1/v2 files into the v3 per-user-key
// envelope.
func newEncryptionSweep(direction encrypt.SweepDirection) *cobra.Command {
	var user string
	var dryRun bool
	verb := "encrypt-all"
	action := "seal"
	past := "sealed"
	requires := "Requires encryption.enabled and a loadable master key; decrypt-all aborts\nat the first file whose key does not match."
	if direction == encrypt.SweepOpen {
		verb = "decrypt-all"
		action = "restore"
		past = "decrypted"
	}
	if direction == encrypt.SweepRotate {
		verb = "rotate-keys"
		action = "re-seal"
		past = "rotated"
		requires = "Plaintext files are skipped (sealing them is encrypt-all's job; files\nit seals land on the current key for free). Requires encryption.enabled,\na loadable master key, and at least one entry in\nencryption.previous_key_paths; the sweep aborts at the first file whose\nkey ID is not in the keyring or whose ring key does not match."
	}
	if direction == encrypt.SweepRekeyV3 {
		verb = "rekey-v3"
		action = "re-seal"
		past = "rekeyed"
		requires = "Plaintext files are skipped (sealing them is encrypt-all's job — in\nper-user mode it seals straight to v3), and files already v3 are skipped.\nRequires encryption.enabled and encryption.per_user_keys; the sweep aborts\nat the first file whose key ID is not in the keyring, whose ring key does\nnot match, or whose per-user key chain cannot unwrap it."
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
			requires,
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
			if direction == encrypt.SweepRotate && len(cfg.Encryption.PreviousKeyPaths) == 0 {
				return fmt.Errorf("ncgo-cli: encryption rotate-keys: encryption.previous_key_paths is empty; nothing to rotate.\n" +
					"Rotation procedure:\n" +
					"  1. ncgo-cli encryption init --key-path <new-key-file>\n" +
					"  2. Move the old master_key_path into encryption.previous_key_paths (append at the end) and set master_key_path to the new key\n" +
					"  3. Restart the server\n" +
					"  4. ncgo-cli encryption rotate-keys")
			}
			if direction == encrypt.SweepRekeyV3 && !cfg.Encryption.PerUserKeys {
				return fmt.Errorf("ncgo-cli: encryption rekey-v3: encryption.per_user_keys is false; the v3 envelope requires per-user keys.\n" +
					"Migration procedure:\n" +
					"  1. Set encryption.per_user_keys: true in the server config and restart (new writes seal as v3; old files keep reading)\n" +
					"  2. ncgo-cli encryption encrypt-all to seal any legacy plaintext (lands on v3)\n" +
					"  3. ncgo-cli encryption rekey-v3 to re-seal v1/v2 files into the v3 envelope")
			}
			current, previous, err := encrypt.LoadKeyring(cfg.Encryption.MasterKeyPath, cfg.Encryption.PreviousKeyPaths)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption: %w", err)
			}
			raw, err := openRawBackend(cfg)
			if err != nil {
				return err
			}
			var resolver encrypt.KeyResolver
			var sqlResolver *encrypt.SQLResolver
			if cfg.Encryption.PerUserKeys {
				// Per-user mode: every sweep subcommand resolves through the
				// per-user key chain — encrypt-all seals straight to v3, and
				// decrypt-all/rotate-keys must read v3 files.
				db, err := openDB(cmd.Context(), cfg)
				if err != nil {
					return err
				}
				defer func() { _ = db.Close() }()
				sqlResolver, err = perUserResolver(db, current, previous)
				if err != nil {
					return err
				}
				resolver = sqlResolver
			}
			enc, err := encrypt.NewWithResolver(current, previous, raw, resolver)
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
			// ADR-0099: a successful rotation also re-seals user keys left
			// under retired ring positions, so old master keys can
			// eventually leave the ring.
			if !dryRun && direction == encrypt.SweepRotate && sqlResolver != nil {
				resealed, err := sqlResolver.ResealUserKeys(cmd.Context())
				if err != nil {
					return fmt.Errorf("ncgo-cli: encryption %s: %w", verb, err)
				}
				fmt.Fprintf(out, "re-sealed %d user key(s) under key id %d\n", resealed, len(previous))
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
			previous := cfg.Encryption.PreviousKeyPaths
			fmt.Fprintf(out, "previous keys: %d configured\n", len(previous))
			for i, p := range previous {
				if _, err := encrypt.LoadMasterKey(p); err != nil {
					fmt.Fprintf(out, "previous key %d (%s): UNUSABLE (%v)\n", i, p, err)
					return fmt.Errorf("ncgo-cli: previous key %s does not load: %w", p, err)
				}
				fmt.Fprintf(out, "previous key %d (%s): OK\n", i, p)
			}
			fmt.Fprintf(out, "current key id: %d\n", len(previous))
			if !cfg.Encryption.Enabled || !cfg.Encryption.PerUserKeys {
				return nil
			}
			// ADR-0099: with per-user keys on, report the key hierarchy's
			// health alongside the key files.
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			current, ring, err := encrypt.LoadKeyring(cfg.Encryption.MasterKeyPath, cfg.Encryption.PreviousKeyPaths)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption: %w", err)
			}
			resolver, err := perUserResolver(db, current, ring)
			if err != nil {
				return err
			}
			inv, err := resolver.Inventory(ctx)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption status: %w", err)
			}
			fmt.Fprintln(out, "per-user keys: on")
			fmt.Fprintf(out, "users: %d total, %d with user key\n", inv.UsersTotal, inv.UsersWithUK)
			if inv.UKsRetiredKeyID > 0 {
				fmt.Fprintf(out, "user keys sealed under retired key ids: %d (rotate-keys re-seals them)\n", inv.UKsRetiredKeyID)
			}
			fmt.Fprintf(out, "file key wraps: %d rows across %d key uuids\n", inv.WrapRows, inv.DistinctKeyUUIDs)
			fmt.Fprintf(out, "v3-sealed files: %d\n", inv.V3Files)
			if inv.StaleUKs > 0 || inv.StaleWraps > 0 {
				fmt.Fprintf(out, "stale key rows (deleted users): %d uks, %d wraps (ncgo-cli encryption reconcile prunes)\n",
					inv.StaleUKs, inv.StaleWraps)
			}
			fmt.Fprintf(out, "broken v3 files (owner wrap missing, file UNREADABLE): %d\n", inv.BrokenV3Files)
			if inv.BrokenV3Files > 0 {
				return fmt.Errorf("ncgo-cli: encryption status: %d v3 file(s) have no owner wrap row and are unreadable (run: ncgo-cli encryption reconcile)", inv.BrokenV3Files)
			}
			return nil
		},
	}
}

// newEncryptionReconcile builds `encryption reconcile`: the ADR-0099 repair
// tool for the per-user key hierarchy. It mints user keys for accounts that
// have none (pre-existing users, imports, the bootstrap admin), wraps file
// keys for the recipients of every existing user/group share (share grant
// hooks only fire for shares created after per-user keys were enabled), and
// prunes key rows whose owning user is gone. Every step is idempotent, so
// the command is safe to re-run after an interruption.
func newEncryptionReconcile() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Repair per-user key state (mint missing user keys, wrap share recipients, prune stale rows)",
		Long: "Repair the per-user key hierarchy (ADR-0099), in three idempotent steps:\n\n" +
			"  1. Mint a user key for every user who has none (accounts created\n" +
			"     before per-user keys or the creation hook existed).\n" +
			"  2. Wrap file keys for the recipients of every existing user and\n" +
			"     group share — grant hooks only wrap shares created after the\n" +
			"     mode was enabled; link and OCM-remote shares have no wrappable\n" +
			"     recipient and are skipped.\n" +
			"  3. Prune key rows whose owning user no longer exists (deletions\n" +
			"     that ran without the lifecycle hook).\n\n" +
			"Safe to re-run at any time; --dry-run reports what would change\n" +
			"without writing anything. Requires encryption.enabled and\n" +
			"encryption.per_user_keys.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if !cfg.Encryption.Enabled || !cfg.Encryption.PerUserKeys {
				return fmt.Errorf("ncgo-cli: encryption reconcile: encryption.per_user_keys is false; reconcile repairs the per-user key hierarchy.\n" +
					"Migration procedure:\n" +
					"  1. Set encryption.per_user_keys: true in the server config and restart (new writes seal as v3; old files keep reading)\n" +
					"  2. ncgo-cli encryption encrypt-all to seal any legacy plaintext (lands on v3)\n" +
					"  3. ncgo-cli encryption rekey-v3 to re-seal v1/v2 files into the v3 envelope\n" +
					"  4. ncgo-cli encryption reconcile to mint user keys, wrap shares, and prune stale rows")
			}
			ctx := cmd.Context()
			current, previous, err := encrypt.LoadKeyring(cfg.Encryption.MasterKeyPath, cfg.Encryption.PreviousKeyPaths)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption: %w", err)
			}
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			resolver, err := perUserResolver(db, current, previous)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			userStore := users.NewSQLStore(db)
			shareStore := sharing.NewSQLShareStore(db)
			keySharer := &files.KeySharer{
				Meta:    files.NewSQLStore(db),
				Wrapper: resolver,
				Shares:  shareStore,
				Users:   userStore,
			}
			allUsers, err := userStore.List(ctx, 0, 0)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption reconcile: list users: %w", err)
			}
			var allShares []files.Share
			for _, u := range allUsers {
				shares, err := shareStore.ListByOwner(ctx, u.ID, "")
				if err != nil {
					return fmt.Errorf("ncgo-cli: encryption reconcile: list shares of %s: %w", u.UID, err)
				}
				allShares = append(allShares, shares...)
			}
			if dryRun {
				inv, err := resolver.Inventory(ctx)
				if err != nil {
					return fmt.Errorf("ncgo-cli: encryption reconcile: %w", err)
				}
				fmt.Fprintf(out, "dry-run reconcile: %d user(s) missing a user key, %d stale user key(s), %d stale wrap(s), %d share(s) to process\n",
					inv.UsersTotal-inv.UsersWithUK, inv.StaleUKs, inv.StaleWraps, len(allShares))
				return nil
			}
			minted, err := resolver.MintMissingUserKeys(ctx)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption reconcile: %w", err)
			}
			var wrapErrs []error
			for i := range allShares {
				if err := keySharer.WrapForShare(ctx, &allShares[i]); err != nil {
					wrapErrs = append(wrapErrs, fmt.Errorf("share %d: %w", allShares[i].ID, err))
				}
			}
			prunedUKs, prunedWraps, err := resolver.PruneStaleKeys(ctx)
			if err != nil {
				return fmt.Errorf("ncgo-cli: encryption reconcile: %w", err)
			}
			fmt.Fprintf(out, "minted %d user key(s)\n", minted)
			fmt.Fprintf(out, "processed %d share(s) (%d error(s))\n", len(allShares), len(wrapErrs))
			fmt.Fprintf(out, "pruned %d user key(s), %d wrap(s)\n", prunedUKs, prunedWraps)
			if len(wrapErrs) > 0 {
				return fmt.Errorf("ncgo-cli: encryption reconcile: %d share(s) failed to wrap: %w",
					len(wrapErrs), errors.Join(wrapErrs...))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what reconcile would do without writing anything")
	return cmd
}
