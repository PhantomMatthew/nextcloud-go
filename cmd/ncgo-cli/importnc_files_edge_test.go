package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestImportNCFilesEdgeCases covers the warning/failure branches: invalid
// uids, users without a files directory, source-vs-target type conflicts,
// special files, unreadable source files, and --verbose output.
func TestImportNCFilesEdgeCases(t *testing.T) {
	cfgPath, storageRoot := cliFilesEnv(t)
	datadir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(datadir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("eve/files/note.txt", "note")
	write("eve/files/conflict", "source file")        // target has a DIRECTORY here
	write("eve/files/conflictdir/inner.txt", "inner") // target has a FILE here
	if err := syscall.Mkfifo(filepath.Join(datadir, "eve", "files", "pipe"), 0o600); err != nil {
		t.Logf("mkfifo unsupported: %v", err)
	}
	unreadable := filepath.Join(datadir, "eve", "files", "unreadable.txt")
	write("eve/files/unreadable.txt", "secret")
	canChmod := os.Geteuid() != 0
	if canChmod {
		if err := os.Chmod(unreadable, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	}

	createTargetUser(t, cfgPath, "eve")
	createTargetUser(t, cfgPath, "dave") // no dave/ in the datadir at all
	ctx := context.Background()
	dav := openTargetDAV(t, cfgPath)
	if _, err := dav.Mkdir(ctx, "eve", "/conflict"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "eve", "/conflictdir", strings.NewReader("target file"), nil); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", importFilesArgs(cfgPath, datadir,
		"--user", "eve", "--user", "dave", "--user", "bad uid", "--verbose")...)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"user eve: create /note.txt",
		"warning: user dave: no files directory in the data directory, skipped",
		"warning: user bad uid: uid not valid for ncgo, skipped",
		"warning: user eve: /conflict: target is a directory, file skipped",
		"warning: user eve: /conflictdir: target exists and is not a directory",
		"users: 1 created, 2 skipped (existing), 0 failed",
	}
	if _, err := os.Stat(filepath.Join(datadir, "eve", "files", "pipe")); err == nil {
		want = append(want, "warning: user eve: /pipe: special file skipped")
	}
	if canChmod {
		want = append(want, "warning: user eve: /unreadable.txt")
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q:\n%s", w, out)
		}
	}
	if got := readStorageFile(t, storageRoot, "eve/note.txt"); got != "note" {
		t.Errorf("note.txt = %q", got)
	}
	// The conflict paths kept their target types and content.
	if _, err := dav.Stat(ctx, "eve", "/conflict"); err != nil {
		t.Errorf("target directory must survive: %v", err)
	}
	rc, _, err := dav.Read(ctx, "eve", "/conflictdir")
	if err != nil {
		t.Fatalf("target file must survive: %v", err)
	}
	_ = rc.Close()
	// inner.txt was not imported (subtree skipped with the failed dir).
	if _, err := dav.Stat(ctx, "eve", "/conflictdir/inner.txt"); err == nil {
		t.Error("inner.txt must not be imported under the file/dir conflict")
	}
}
