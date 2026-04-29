package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(64)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "import-har":
		os.Exit(runImportHAR(args))
	case "lint":
		os.Exit(runLint(args))
	case "accept":
		os.Exit(runAccept(args))
	case "migrate":
		os.Exit(runMigrate(args))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "golden-gen: unknown subcommand %q\n", cmd)
		usage()
		os.Exit(64)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `golden-gen - golden test case authoring tool

Usage:
  golden-gen import-har  -in FILE -out DIR  Import mitmproxy HAR into golden cases
  golden-gen lint        [-root DIR]        Validate golden cases against schema
  golden-gen accept      -case DIR          Update response.http after intentional change
  golden-gen migrate     [-root DIR]        Migrate cases to current schema_version`)
}

func runImportHAR(args []string) int {
	fs := flag.NewFlagSet("import-har", flag.ContinueOnError)
	in := fs.String("in", "", "HAR file path")
	out := fs.String("out", "", "output directory under testdata/golden")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "import-har: -in and -out required")
		return 64
	}
	fmt.Fprintln(os.Stderr, "import-har: not yet implemented (Phase 0 stub)")
	return 70
}

func runLint(args []string) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	root := fs.String("root", "testdata/golden", "golden cases root")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	info, err := os.Stat(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint: stat %s: %v\n", *root, err)
		return 66
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "lint: %s is not a directory\n", *root)
		return 66
	}
	fmt.Fprintf(os.Stderr, "lint: scanned %s (full validation not yet implemented)\n", *root)
	return 0
}

func runAccept(args []string) int {
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	caseDir := fs.String("case", "", "case directory")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *caseDir == "" {
		fmt.Fprintln(os.Stderr, "accept: -case required")
		return 64
	}
	fmt.Fprintln(os.Stderr, "accept: not yet implemented (Phase 0 stub)")
	return 70
}

func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	root := fs.String("root", "testdata/golden", "golden cases root")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	fmt.Fprintf(os.Stderr, "migrate: scanned %s (no migrations yet, schema_version=1)\n", *root)
	return 0
}
