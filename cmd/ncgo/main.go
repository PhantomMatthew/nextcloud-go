package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/app"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "ncgo",
		Short:         "nextcloud-go server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServe(), newVersion())
	return root
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "ncgo %s (commit %s, built %s)\n",
				observability.Version, observability.Commit, observability.BuildDate)
		},
	}
}

func newServe() *cobra.Command {
	var (
		cfgPath string
		addr    string
		dev     bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var (
				cfg *config.Config
				err error
			)
			if dev {
				cfg = app.DevConfig()
			} else {
				cfg, err = config.Load(config.LoadOptions{Path: cfgPath})
				if err != nil {
					return err
				}
			}
			if addr != "" {
				cfg.Server.Listen = addr
			}
			logger := newLogger(cfg)
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			a, err := app.New(ctx, cfg, logger)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close(ctx) }()
			return a.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to YAML config")
	cmd.Flags().StringVar(&addr, "addr", "", "listen address override")
	cmd.Flags().BoolVar(&dev, "dev", false, "in-memory SQLite with bootstrap admin")
	return cmd
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.Observability.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Observability.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
