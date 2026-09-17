package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins"
)

func newPlugin() *cobra.Command {
	cmd := &cobra.Command{Use: "plugin", Short: "Plugin tools"}
	cmd.AddCommand(&cobra.Command{
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
	})
	return cmd
}
