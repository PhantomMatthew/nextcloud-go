package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// cliFilesEnv creates a migrated temp sqlite database plus a localfs storage
// root, and a config file pointing at both.
func cliFilesEnv(t *testing.T) (cfgPath, storageRoot string) {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "cli.db")
	storageRoot = filepath.Join(dir, "storage")
	cfgPath = filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`
database:
  driver: sqlite
  dsn: %q
storage:
  default_backend: local
  backends:
    local:
      type: localfs
      root: %q
auth:
  argon2id:
    memory_kb: 8
    iterations: 1
    parallelism: 1
`, dsn, storageRoot)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{Driver: database.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return cfgPath, storageRoot
}

// importDatadirEnv builds a PHP Nextcloud data directory fixture and returns
// the datadir path plus the fixture file mtimes (unix seconds).
func importDatadirEnv(t *testing.T) (string, map[string]int64) {
	t.Helper()
	dir := t.TempDir()
	mtimes := map[string]int64{
		"a.txt":     1600000001,
		"sub/b.txt": 1600000002,
		".hidden":   1600000003,
	}
	write := func(rel, content string, mtime int64) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if mtime > 0 {
			mt := time.Unix(mtime, 0)
			if err := os.Chtimes(p, mt, mt); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("alice/files/a.txt", "hello alice", mtimes["a.txt"])
	write("alice/files/sub/b.txt", "bee", mtimes["sub/b.txt"])
	write("alice/files/.hidden", "hid", mtimes[".hidden"])
	// Nextcloud-internal siblings: must never be imported.
	write("alice/files_trashbin/deleted.txt", "trash", 0)
	write("alice/files_versions/old", "version", 0)
	write("alice/cache/scan", "cache", 0)
	// bob has files but no target user; carol is server-side encrypted.
	write("bob/files/x.txt", "bob", 0)
	write("carol/files/y.txt", "carol", 0)
	write("carol/files_encryption/keys/k", "key", 0)
	// Instance-level directory that happens to contain a files/ subdir.
	write("appdata_oc123456/files/plugin.dat", "appdata", 0)
	// A symlink is skipped with a warning (never followed).
	if err := os.Symlink("a.txt", filepath.Join(dir, "alice", "files", "link.txt")); err != nil {
		t.Logf("symlink unsupported: %v", err)
	}
	return dir, mtimes
}

func importFilesArgs(cfgPath, datadir string, extra ...string) []string {
	args := make([]string, 0, 6+len(extra))
	args = append(args, "--config", cfgPath, "import-nextcloud", "files", "--datadir", datadir)
	return append(args, extra...)
}

// openTargetDAV builds the files DAV against the target config, mirroring the
// CLI wiring, for post-import assertions.
func openTargetDAV(t *testing.T, cfgPath string) *files.DAV {
	t.Helper()
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := openStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return filesDAV(st, db)
}

func countRows(t *testing.T, cfgPath, table string) int {
	t.Helper()
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(context.Background(), `SELECT COUNT(*) FROM `+table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("count query returned no rows")
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func createTargetUser(t *testing.T, cfgPath, uid string) {
	t.Helper()
	store := openTargetStore(t, cfgPath)
	if err := store.Create(context.Background(), &users.User{
		UID: uid, DisplayName: uid, PasswordHash: "x", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func readStorageFile(t *testing.T, storageRoot, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(storageRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestImportNCFiles(t *testing.T) {
	cfgPath, storageRoot := cliFilesEnv(t)
	datadir, mtimes := importDatadirEnv(t)
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "carol")

	out, err := runCLI(t, "", importFilesArgs(cfgPath, datadir)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"user alice: 3 files created, 0 updated, 1 skipped, 0 failed, 1 directories created",
		"users: 1 created, 2 skipped (existing), 0 failed",
		"files: 3 created, 1 skipped (existing), 0 failed",
		"directories: 1 created, 0 skipped (existing), 0 failed",
		"warning: user bob: not in target (run 'import-nextcloud users' first), skipped",
		"warning: user carol: server-side encrypted source not supported, user skipped",
		"warning: user alice: /link.txt: symlink skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Content lands in the storage backend under alice/..., dotfiles and
	// nested directories included; Nextcloud-internal siblings do not.
	if got := readStorageFile(t, storageRoot, "alice/a.txt"); got != "hello alice" {
		t.Errorf("a.txt = %q", got)
	}
	if got := readStorageFile(t, storageRoot, "alice/sub/b.txt"); got != "bee" {
		t.Errorf("b.txt = %q", got)
	}
	if got := readStorageFile(t, storageRoot, "alice/.hidden"); got != "hid" {
		t.Errorf(".hidden = %q", got)
	}
	for _, absent := range []string{
		"alice/files_trashbin", "alice/files_versions", "alice/cache",
		"bob", "carol", "appdata_oc123456",
	} {
		if _, err := os.Stat(filepath.Join(storageRoot, filepath.FromSlash(absent))); !os.IsNotExist(err) {
			t.Errorf("%s must not be imported (stat err = %v)", absent, err)
		}
	}

	// The DAV sees the imported tree with source mtimes (second resolution).
	ctx := context.Background()
	dav := openTargetDAV(t, cfgPath)
	for rel, wantUnix := range mtimes {
		e, err := dav.Stat(ctx, "alice", "/"+rel)
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if e.ModTime.Unix() != wantUnix {
			t.Errorf("%s mtime = %d, want %d", rel, e.ModTime.Unix(), wantUnix)
		}
	}
	rc, _, err := dav.Read(ctx, "alice", "/sub/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	kids, err := dav.List(ctx, "alice", "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 3 { // a.txt, sub, .hidden — link.txt absent (symlink skipped)
		names := make([]string, 0, len(kids))
		for _, k := range kids {
			names = append(names, k.Path)
		}
		t.Errorf("root children = %v, want 3 entries", names)
	}

	// Second run: everything unchanged is skipped, and no version snapshots
	// are created (no Write calls happen for unchanged files).
	out, err = runCLI(t, "", importFilesArgs(cfgPath, datadir)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"files: 0 created, 4 skipped (existing), 0 failed",
		"directories: 0 created, 1 skipped (existing), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "file_versions"); n != 0 {
		t.Errorf("unchanged re-import must not snapshot versions, got %d rows", n)
	}

	// A file whose size changed is updated through the DAV pipeline, which
	// snapshots the previous version exactly like a client overwrite.
	changed := filepath.Join(datadir, "alice", "files", "a.txt")
	if err := os.WriteFile(changed, []byte("hello alice, edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(1600000999, 0)
	if err := os.Chtimes(changed, mt, mt); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "", importFilesArgs(cfgPath, datadir)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "files: 0 created, 1 updated, 3 skipped (existing), 0 failed") {
		t.Errorf("update run output:\n%s", out)
	}
	if got := readStorageFile(t, storageRoot, "alice/a.txt"); got != "hello alice, edited" {
		t.Errorf("updated a.txt = %q", got)
	}
	e, err := dav.Stat(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.ModTime.Unix() != 1600000999 {
		t.Errorf("updated mtime = %d", e.ModTime.Unix())
	}
	if n := countRows(t, cfgPath, "file_versions"); n != 1 {
		t.Errorf("overwrite must snapshot one version, got %d rows", n)
	}
}

func TestImportNCFilesDryRun(t *testing.T) {
	cfgPath, storageRoot := cliFilesEnv(t)
	datadir, _ := importDatadirEnv(t)
	createTargetUser(t, cfgPath, "alice")

	out, err := runCLI(t, "", importFilesArgs(cfgPath, datadir, "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"files: 3 created, 1 skipped (existing), 0 failed",
		"directories: 1 created, 0 skipped (existing), 0 failed",
		"dry-run: no changes written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(storageRoot, "alice")); !os.IsNotExist(err) {
		t.Errorf("dry-run must not write storage (stat err = %v)", err)
	}
	if n := countRows(t, cfgPath, "files"); n != 0 {
		t.Errorf("dry-run must not write the filecache, got %d rows", n)
	}
}

func TestImportNCFilesUserFlag(t *testing.T) {
	cfgPath, storageRoot := cliFilesEnv(t)
	datadir, _ := importDatadirEnv(t)
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "carol")

	out, err := runCLI(t, "", importFilesArgs(cfgPath, datadir, "--user", "carol")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: user carol: server-side encrypted source not supported, user skipped") {
		t.Errorf("output missing carol warning:\n%s", out)
	}
	if strings.Contains(out, "user alice:") {
		t.Errorf("--user carol must not touch alice:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(storageRoot, "alice")); !os.IsNotExist(err) {
		t.Errorf("alice must not be imported (stat err = %v)", err)
	}

	out, err = runCLI(t, "", importFilesArgs(cfgPath, datadir, "--user", "alice")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "user alice: 3 files created") {
		t.Errorf("output missing alice line:\n%s", out)
	}
	if strings.Contains(out, "carol") {
		t.Errorf("--user alice must not touch carol:\n%s", out)
	}
	if got := readStorageFile(t, storageRoot, "alice/a.txt"); got != "hello alice" {
		t.Errorf("a.txt = %q", got)
	}
}

func TestImportNCFilesFlagValidation(t *testing.T) {
	cfgPath, _ := cliFilesEnv(t)

	if _, err := runCLI(t, "", "--config", cfgPath, "import-nextcloud", "files"); err == nil ||
		!strings.Contains(err.Error(), "--datadir is required") {
		t.Errorf("missing datadir = %v", err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "import-nextcloud", "files",
		"--datadir", filepath.Join(os.TempDir(), "ncgo-no-such-datadir")); err == nil ||
		!strings.Contains(err.Error(), "--datadir") {
		t.Errorf("unreadable datadir = %v", err)
	}
}
