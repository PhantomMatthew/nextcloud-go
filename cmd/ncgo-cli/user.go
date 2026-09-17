package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

func newUser() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "User administration"}
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
			store := users.NewSQLStore(db)
			h := auth.NewArgon2id(auth.Argon2idParams{
				MemoryKB:    cfg.Auth.Argon2id.MemoryKB,
				Iterations:  cfg.Auth.Argon2id.Iterations,
				Parallelism: cfg.Auth.Argon2id.Parallelism,
			})
			hash, err := h.Hash(pw)
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
	cmd.AddCommand(add)
	return cmd
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
