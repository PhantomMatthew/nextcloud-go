package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// cliEnv creates a migrated temp sqlite database and a minimal config file
// pointing at it, returning the config path.
func cliEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "cli.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`
database:
  driver: sqlite
  dsn: %q
auth:
  argon2id:
    memory_kb: 8
    iterations: 1
    parallelism: 1
`, dsn)
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
	return cfgPath
}

func runCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestUserCommands(t *testing.T) {
	cfgPath := cliEnv(t)

	if _, err := runCLI(t, "", "--config", cfgPath, "user", "add", "alice"); err == nil {
		t.Fatal("add without --password-stdin must fail")
	}
	if _, err := runCLI(t, "secret\n", "--config", cfgPath, "user", "add", "alice", "--display-name", "Alice A", "--password-stdin"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "pw\n", "--config", cfgPath, "user", "add", "bob", "--password-stdin"); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", "--config", cfgPath, "user", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"UID", "alice", "Alice A", "bob", "true"} {
		if !strings.Contains(out, want) {
			t.Errorf("user list missing %q:\n%s", want, out)
		}
	}

	out, err = runCLI(t, "", "--config", cfgPath, "user", "disable", "bob")
	if err != nil || !strings.Contains(out, "disabled bob") {
		t.Fatalf("disable = %q %v", out, err)
	}
	out, err = runCLI(t, "", "--config", cfgPath, "user", "list", "--limit", "1", "--offset", "1")
	if err != nil || !strings.Contains(out, "bob") || strings.Contains(out, "alice") {
		t.Fatalf("list page = %q %v", out, err)
	}
	if !strings.Contains(out, "false") {
		t.Errorf("disabled user must list as false:\n%s", out)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "user", "enable", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "user", "disable", "missing"); err == nil ||
		!strings.Contains(err.Error(), `unknown user "missing"`) {
		t.Fatalf("disable unknown = %v", err)
	}

	out, err = runCLI(t, "newpass\n", "--config", cfgPath, "user", "reset-password", "alice", "--password-stdin")
	if err != nil || !strings.Contains(out, "password reset for alice") {
		t.Fatalf("reset-password = %q %v", out, err)
	}
	if _, err := runCLI(t, "x\n", "--config", cfgPath, "user", "reset-password", "missing", "--password-stdin"); err == nil {
		t.Fatal("reset-password unknown user must fail")
	}

	if _, err := runCLI(t, "", "--config", cfgPath, "user", "delete", "bob"); err == nil {
		t.Fatal("delete without --yes must fail")
	}
	out, err = runCLI(t, "", "--config", cfgPath, "user", "delete", "bob", "--yes")
	if err != nil || !strings.Contains(out, "deleted bob") {
		t.Fatalf("delete = %q %v", out, err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "user", "delete", "bob", "--yes"); err == nil {
		t.Fatal("deleting a missing user must fail")
	}
	out, err = runCLI(t, "", "--config", cfgPath, "user", "list")
	if err != nil || strings.Contains(out, "bob") {
		t.Fatalf("list after delete = %q %v", out, err)
	}

	// The reset password must actually authenticate.
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := openDB(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	store := users.NewSQLStore(db)
	h := auth.NewArgon2id(auth.Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
	v := users.NewPasswordVerifier(store, h)
	if _, err := v.Verify(ctx, "alice", "newpass"); err != nil {
		t.Errorf("new password does not verify: %v", err)
	}
	if _, err := v.Verify(ctx, "alice", "secret"); err == nil {
		t.Error("old password still verifies")
	}
}

func TestGroupCommands(t *testing.T) {
	cfgPath := cliEnv(t)
	for _, uid := range []string{"alice", "bob"} {
		if _, err := runCLI(t, "pw\n", "--config", cfgPath, "user", "add", uid, "--password-stdin"); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runCLI(t, "", "--config", cfgPath, "group", "add", "team", "--display-name", "Team")
	if err != nil || !strings.Contains(out, "created group team") {
		t.Fatalf("group add = %q %v", out, err)
	}
	out, err = runCLI(t, "", "--config", cfgPath, "group", "list")
	if err != nil || !strings.Contains(out, "GID") || !strings.Contains(out, "team") || !strings.Contains(out, "Team") {
		t.Fatalf("group list = %q %v", out, err)
	}

	if _, err := runCLI(t, "", "--config", cfgPath, "group", "adduser", "team", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "group", "adduser", "team", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "group", "adduser", "missing", "alice"); err == nil {
		t.Fatal("adduser to unknown group must fail")
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "group", "adduser", "team", "missing"); err == nil {
		t.Fatal("adduser of unknown user must fail")
	}

	out, err = runCLI(t, "", "--config", cfgPath, "group", "members", "team")
	if err != nil || !strings.Contains(out, "alice") || !strings.Contains(out, "bob") {
		t.Fatalf("members = %q %v", out, err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "group", "members", "missing"); err == nil ||
		!strings.Contains(err.Error(), `unknown group "missing"`) {
		t.Fatalf("members of unknown group = %v", err)
	}

	out, err = runCLI(t, "", "--config", cfgPath, "group", "removeuser", "team", "bob")
	if err != nil || !strings.Contains(out, "removed bob from team") {
		t.Fatalf("removeuser = %q %v", out, err)
	}
	out, err = runCLI(t, "", "--config", cfgPath, "group", "members", "team")
	if err != nil || strings.Contains(out, "bob") {
		t.Fatalf("members after removeuser = %q %v", out, err)
	}

	if _, err := runCLI(t, "", "--config", cfgPath, "group", "delete", "team"); err == nil {
		t.Fatal("group delete without --yes must fail")
	}
	out, err = runCLI(t, "", "--config", cfgPath, "group", "delete", "team", "--yes")
	if err != nil || !strings.Contains(out, "deleted group team") {
		t.Fatalf("group delete = %q %v", out, err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "group", "members", "team"); err == nil {
		t.Fatal("members of deleted group must fail")
	}
}

func TestConfigCommands(t *testing.T) {
	cfgPath := cliEnv(t)

	out, err := runCLI(t, "", "--config", cfgPath, "config", "get", "core", "theme")
	if err == nil || out != "" {
		t.Fatalf("get of unset key must fail with empty stdout, got %q %v", out, err)
	}

	out, err = runCLI(t, "", "--config", cfgPath, "config", "set", "plugin", "webhook-forwarder.webhook.url", "https://example.com/hook")
	if err != nil || !strings.Contains(out, "set plugin webhook-forwarder.webhook.url") {
		t.Fatalf("set = %q %v", out, err)
	}
	out, err = runCLI(t, "", "--config", cfgPath, "config", "get", "plugin", "webhook-forwarder.webhook.url")
	if err != nil || strings.TrimSpace(out) != "https://example.com/hook" {
		t.Fatalf("get = %q %v", out, err)
	}

	out, err = runCLI(t, "", "--config", cfgPath, "config", "delete", "plugin", "webhook-forwarder.webhook.url")
	if err != nil || !strings.Contains(out, "deleted plugin webhook-forwarder.webhook.url") {
		t.Fatalf("delete = %q %v", out, err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "config", "delete", "plugin", "never-set"); err != nil {
		t.Errorf("deleting an unset key must not fail: %v", err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "config", "get", "plugin", "webhook-forwarder.webhook.url"); err == nil {
		t.Fatal("get after delete must fail")
	}
}
