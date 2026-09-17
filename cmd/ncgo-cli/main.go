package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
)

var cfgPath string

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "ncgo-cli",
		Short:         "Operator CLI for nextcloud-go",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "", "path to YAML config")
	root.AddCommand(newMigrate(), newUser(), newPlugin(), newVersion())
	return root
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "ncgo-cli %s (commit %s, built %s)\n",
				observability.Version, observability.Commit, observability.BuildDate)
		},
	}
}
