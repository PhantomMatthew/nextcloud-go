package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/appconfig"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "App configuration",
		Long: "Read and write rows in the appconfig table.\n\n" +
			"Plugin configuration lives under appid \"plugin\" with keys of the\n" +
			"form \"<plugin_id>.<key>\" (e.g. config set plugin webhook-forwarder.webhook.url https://...).",
	}
	cmd.AddCommand(newConfigGet(), newConfigSet(), newConfigDelete())
	return cmd
}

func newConfigGet() *cobra.Command {
	return &cobra.Command{
		Use:   "get <appid> <key>",
		Short: "Print a config value",
		Long: "Print the value stored under (appid, key).\n\n" +
			"If the key is not set, nothing is printed to stdout and the exit\n" +
			"code is 1, so the command is safe to use in shell scripts.",
		Args: cobra.ExactArgs(2),
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
			val, err := appconfig.NewStore(db).Get(ctx, args[0], args[1])
			if err != nil {
				if errors.Is(err, appconfig.ErrNotFound) {
					return fmt.Errorf("ncgo-cli: %w", err)
				}
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), val)
			return nil
		},
	}
}

func newConfigSet() *cobra.Command {
	return &cobra.Command{
		Use:   "set <appid> <key> <value>",
		Short: "Set a config value (insert or replace)",
		Args:  cobra.ExactArgs(3),
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
			if err := appconfig.NewStore(db).Set(ctx, args[0], args[1], args[2]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s %s\n", args[0], args[1])
			return nil
		},
	}
}

func newConfigDelete() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <appid> <key>",
		Short: "Delete a config value (a missing key is not an error)",
		Args:  cobra.ExactArgs(2),
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
			if err := appconfig.NewStore(db).Delete(ctx, args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s %s\n", args[0], args[1])
			return nil
		},
	}
}
