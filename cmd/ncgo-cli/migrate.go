package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
)

func newMigrate() *cobra.Command {
	cmd := &cobra.Command{Use: "migrate", Short: "Schema migrations"}
	cmd.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd.Context(), func(ctx context.Context, db database.DB) error {
				s, ok := database.Unwrap(db)
				if !ok {
					return fmt.Errorf("ncgo-cli: unwrap db")
				}
				n, err := migrations.Up(ctx, s, db.Dialect(), slog.Default())
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "applied %d\n", n)
				return nil
			})
		},
	})
	down := &cobra.Command{
		Use:   "down",
		Short: "Roll back migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			steps, err := cmd.Flags().GetInt("steps")
			if err != nil {
				return err
			}
			return withDB(cmd.Context(), func(ctx context.Context, db database.DB) error {
				s, ok := database.Unwrap(db)
				if !ok {
					return fmt.Errorf("ncgo-cli: unwrap db")
				}
				return migrations.Down(ctx, s, db.Dialect(), steps, slog.Default())
			})
		},
	}
	down.Flags().Int("steps", 1, "number of versions to roll back")
	cmd.AddCommand(down)
	cmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print schema version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(cmd.Context(), func(ctx context.Context, db database.DB) error {
				s, ok := database.Unwrap(db)
				if !ok {
					return fmt.Errorf("ncgo-cli: unwrap db")
				}
				v, dirty, err := migrations.Version(ctx, s, db.Dialect())
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%d dirty=%v\n", v, dirty)
				return nil
			})
		},
	})
	return cmd
}

func withDB(ctx context.Context, fn func(context.Context, database.DB) error) error {
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		return err
	}
	db, err := database.Open(ctx, database.Config{
		Driver:          database.Dialect(cfg.Database.Driver),
		DSN:             cfg.Database.DSN,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return fn(ctx, db)
}
