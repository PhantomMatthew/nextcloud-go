package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

func newUser() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "User administration"}
	cmd.AddCommand(
		newUserAdd(),
		newUserList(),
		newUserSetEnabled("enable <uid>", true),
		newUserSetEnabled("disable <uid>", false),
		newUserDelete(),
		newUserResetPassword(),
	)
	return cmd
}

func passwordHasher(cfg *config.Config) *auth.Argon2id {
	return auth.NewArgon2id(auth.Argon2idParams{
		MemoryKB:    cfg.Auth.Argon2id.MemoryKB,
		Iterations:  cfg.Auth.Argon2id.Iterations,
		Parallelism: cfg.Auth.Argon2id.Parallelism,
	})
}

func newUserAdd() *cobra.Command {
	var display string
	var stdin bool
	add := &cobra.Command{
		Use:   "add <uid>",
		Short: "Create a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pw, err := readPassword(cmd, stdin)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			store := users.NewSQLStore(db)
			hook, err := userKeysHook(cfg, db)
			if err != nil {
				return err
			}
			store.UserKeys = hook
			hash, err := passwordHasher(cfg).Hash(pw)
			if err != nil {
				return err
			}
			name := display
			if name == "" {
				name = args[0]
			}
			return store.Create(ctx, &users.User{UID: args[0], DisplayName: name, PasswordHash: hash, Enabled: true})
		},
	}
	add.Flags().StringVar(&display, "display-name", "", "display name")
	add.Flags().BoolVar(&stdin, "password-stdin", false, "read password from stdin")
	return add
}

func newUserList() *cobra.Command {
	var limit, offset int
	list := &cobra.Command{
		Use:   "list",
		Short: "List users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			rows, err := users.NewSQLStore(db).List(ctx, limit, offset)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "UID\tDISPLAY NAME\tENABLED")
			for _, u := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%t\n", u.UID, u.DisplayName, u.Enabled)
			}
			return tw.Flush()
		},
	}
	list.Flags().IntVar(&limit, "limit", 0, "maximum users to list (0 = all)")
	list.Flags().IntVar(&offset, "offset", 0, "skip this many users")
	return list
}

func newUserSetEnabled(use string, enabled bool) *cobra.Command {
	verb := strings.Split(use, " ")[0]
	return &cobra.Command{
		Use:   use,
		Short: verb + " a user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := users.NewSQLStore(db).SetEnabled(ctx, args[0], enabled); err != nil {
				if errors.Is(err, users.ErrNotFound) {
					return unknownUserErr(args[0])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%sd %s\n", verb, args[0])
			return nil
		},
	}
}

func newUserDelete() *cobra.Command {
	var yes bool
	del := &cobra.Command{
		Use:   "delete <uid>",
		Short: "Delete a user",
		Long: "Delete a user and their group memberships.\n\n" +
			"Files, shares, and other data owned by the user are NOT removed;\n" +
			"reassign or purge them first (mirroring occ user:delete warnings).\n\n" +
			"When per-user encryption is on, deleting a user also deletes their\n" +
			"per-user key rows: any sealed files they still own become\n" +
			"UNREADABLE — reassign or purge those files first.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("ncgo-cli: user delete requires --yes")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			store := users.NewSQLStore(db)
			hook, err := userKeysHook(cfg, db)
			if err != nil {
				return err
			}
			store.UserKeys = hook
			if err := store.Delete(ctx, args[0]); err != nil {
				if errors.Is(err, users.ErrNotFound) {
					return unknownUserErr(args[0])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
	del.Flags().BoolVar(&yes, "yes", false, "confirm the deletion (required)")
	return del
}

func newUserResetPassword() *cobra.Command {
	var stdin bool
	var force bool
	reset := &cobra.Command{
		Use:   "reset-password <uid>",
		Short: "Set a new password for a user",
		Long: "Set a new password for a user.\n\n" +
			"WARNING (ADR-0100): for a user enrolled in password-wrapped encryption keys,\n" +
			"the old password is the only way to unwrap their private key, so resetting\n" +
			"the password makes their sealed files PERMANENTLY UNREADABLE. The command\n" +
			"refuses enrolled users unless --force is given; --force destroys their key\n" +
			"enrollment and all their file-key wrap rows before setting the new password.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pw, err := readPassword(cmd, stdin)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			store := users.NewSQLStore(db)
			u, err := store.GetByUID(ctx, args[0])
			if err != nil {
				if errors.Is(err, users.ErrNotFound) {
					return unknownUserErr(args[0])
				}
				return err
			}
			// ADR-0100: an enrolled user's key is sealed under the OLD
			// password — refuse unless the operator explicitly destroys the
			// enrollment first.
			hook, err := userKeysHook(cfg, db)
			if err != nil {
				return err
			}
			if resolver, ok := hook.(*encrypt.SQLResolver); ok {
				enrolled, err := resolver.Enrolled(ctx, args[0])
				if err != nil {
					return err
				}
				if enrolled && !force {
					return fmt.Errorf("ncgo-cli: user %s has password-wrapped encryption keys (ADR-0100); resetting the password makes their sealed files PERMANENTLY UNREADABLE. Re-run with --force to destroy their key enrollment and all wrap rows", args[0])
				}
				if enrolled {
					if err := resolver.DestroyEnrollment(ctx, args[0]); err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "DATA LOSS: destroyed password-wrapped key enrollment and all file-key wrap rows for %s; their existing v3-sealed files are PERMANENTLY UNREADABLE\n", args[0])
				}
			}
			hash, err := passwordHasher(cfg).Hash(pw)
			if err != nil {
				return err
			}
			if err := store.UpdatePasswordHash(ctx, u.ID, hash); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "password reset for %s\n", args[0])
			return nil
		},
	}
	reset.Flags().BoolVar(&stdin, "password-stdin", false, "read password from stdin")
	reset.Flags().BoolVar(&force, "force", false, "destroy an enrolled user's password-wrapped key enrollment and all wrap rows (PERMANENT DATA LOSS)")
	return reset
}

func readPassword(cmd *cobra.Command, stdin bool) (string, error) {
	if !stdin {
		return "", fmt.Errorf("ncgo-cli: --password-stdin required")
	}
	b, err := io.ReadAll(bufio.NewReader(cmd.InOrStdin()))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
