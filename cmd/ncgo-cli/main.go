package main

import (
	"fmt"
	"os"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
)

func main() {
	fmt.Fprintf(os.Stderr, "ncgo-cli %s: not yet implemented (Phase 0 skeleton)\n", observability.Version)
	os.Exit(64)
}
