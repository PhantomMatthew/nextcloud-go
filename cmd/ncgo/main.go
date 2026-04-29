package main

import (
	"fmt"
	"os"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
)

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("ncgo %s (commit %s, built %s)\n",
			observability.Version, observability.Commit, observability.BuildDate)
		return
	}
	fmt.Fprintln(os.Stderr, "ncgo: server not yet implemented (Phase 0 skeleton)")
	os.Exit(64)
}
