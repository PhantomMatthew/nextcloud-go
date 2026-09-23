package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// testInstanceSecret stands in for the source instance's config.php 'secret'
// that the operator copied into ncgo's instance.secret.
const testInstanceSecret = "test-instance-secret"

// rawAliceToken is the original Nextcloud app password (72 chars, the
// TokenLength both sides generate) a client presents after migration.
const rawAliceToken = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// importTokensSourceEnv creates a temp sqlite database holding the PHP
// Nextcloud <prefix>users and <prefix>authtoken tables with one fixture row
// per supported and skipped category.
func importTokensSourceEnv(t *testing.T, prefix string) string {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "source.db")
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{Driver: database.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TABLE ` + prefix + `users (uid TEXT PRIMARY KEY, uid_lower TEXT, displayname TEXT, password TEXT)`,
		`CREATE TABLE ` + prefix + `authtoken (id INTEGER PRIMARY KEY, uid TEXT, login_name TEXT,
			name TEXT, token TEXT, type INTEGER, last_activity INTEGER)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range [][2]string{
		{"alice", "alice"},
		{"ghost", "ghost"}, // maps, but absent from the target
		{"Legacy User", "legacyuser"},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`users (uid, uid_lower) VALUES (?, ?)`, u[0], u[1]); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) *string { return &s }
	i64 := func(v int64) *int64 { return &v }
	type tokenRow struct {
		uid          string
		loginName    *string
		name         *string
		token        *string
		tokenType    int
		lastActivity *int64
	}
	rows := []tokenRow{
		// 1: alice's app password (created), hash computed exactly like
		// Nextcloud's PublicKeyTokenProvider does.
		{"alice", str("alice"), str("Alice's phone"), str(auth.HashToken(rawAliceToken, testInstanceSecret)), 1, i64(1600000000)},
		// 2: browser session (type 0) — never selected.
		{"alice", str("alice"), str("Firefox"), str("session-hash-value"), 0, i64(1600000001)},
		// 3: wipe token (type 2) — never selected.
		{"alice", str("alice"), str("wipe"), str("wipe-hash-value"), 2, i64(1600000002)},
		// 4: ghost maps but is absent from the target (skipped).
		{"ghost", str("ghost"), str("Ghost app"), str("ghost-hash-value"), 1, i64(1600000003)},
		// 5: nouser is not in oc_users at all (skipped).
		{"nouser", str("nouser"), str("No user"), str("nouser-hash-value"), 1, i64(1600000004)},
		// 6: uid fallback — "Legacy User" imports as legacyuser; NULL
		// login_name falls back to the mapped uid, NULL name to "", NULL
		// last_activity to 0 (created).
		{"Legacy User", nil, nil, str("legacy-hash-value"), 1, nil},
		// 7: NULL token (skipped).
		{"alice", str("alice"), str("empty"), nil, 1, i64(1600000005)},
	}
	for i, tk := range rows {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`authtoken
			(id, uid, login_name, name, token, type, last_activity)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			i+1, tk.uid, tk.loginName, tk.name, tk.token, tk.tokenType, tk.lastActivity); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func importTokensArgs(cfgPath, dsn string, extra ...string) []string {
	args := make([]string, 0, 8+len(extra))
	args = append(args,
		"--config", cfgPath, "import-nextcloud", "tokens",
		"--source-driver", "sqlite", "--source-dsn", dsn,
	)
	return append(args, extra...)
}

// openTargetAuthStore builds the app-password store against the target config
// for post-import assertions.
func openTargetAuthStore(t *testing.T, cfgPath string) *auth.SQLStore {
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
	return auth.NewSQLStore(db)
}

func TestImportNCTokens(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importTokensSourceEnv(t, "oc_")
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "legacyuser")

	out, err := runCLI(t, "", importTokensArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"app passwords: 2 created, 3 skipped (existing), 0 failed",
		"warning: token 4: user ghost not in target (run 'import-nextcloud users' first), skipped",
		"warning: token 5: user nouser not imported, skipped",
		"warning: token 7: empty token hash, skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	ctx := context.Background()
	as := openTargetAuthStore(t, cfgPath)

	// The imported row carries the verbatim hash and the mapped fields.
	aliceHash := auth.HashToken(rawAliceToken, testInstanceSecret)
	tok, err := as.GetByHash(ctx, aliceHash)
	if err != nil {
		t.Fatal(err)
	}
	if tok.ID != "nc-1" {
		t.Errorf("token id = %q, want nc-1", tok.ID)
	}
	if tok.UID != "alice" || tok.LoginName != "alice" || tok.Name != "Alice's phone" {
		t.Errorf("alice token = %+v", tok)
	}
	if tok.Type != auth.TokenTypePermanent {
		t.Errorf("token type = %d, want %d (permanent)", tok.Type, auth.TokenTypePermanent)
	}
	if tok.CreatedAt.UnixMilli() != 1600000000000 {
		t.Errorf("created_at = %d ms, want 1600000000000 (last_activity seconds)", tok.CreatedAt.UnixMilli())
	}

	// Money test: ncgo's app-password verifier configured with the source
	// instance's secret authenticates the original token — the copied
	// oc_authtoken hash verifies byte-identically on the ncgo side.
	v := auth.NewAppPasswordVerifier(as, testInstanceSecret)
	p, err := v.Verify(ctx, "alice", rawAliceToken)
	if err != nil {
		t.Fatalf("imported app password does not verify with the source secret: %v", err)
	}
	if p.UID != "alice" || p.AuthMethod != auth.AuthMethodAppPassword {
		t.Errorf("principal = %+v", p)
	}
	// ... and only with that secret: a fresh-secret ncgo rejects it.
	fresh := auth.NewAppPasswordVerifier(as, "a-different-secret")
	if _, err := fresh.Verify(ctx, "alice", rawAliceToken); err == nil {
		t.Error("token must not verify against a different instance.secret")
	}

	// The uid_lower fallback row lands for the mapped uid with the NULL
	// tolerances applied.
	legacy, err := as.GetByHash(ctx, "legacy-hash-value")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ID != "nc-6" || legacy.UID != "legacyuser" {
		t.Errorf("legacy token = %+v, want nc-6 for legacyuser", legacy)
	}
	if legacy.LoginName != "legacyuser" || legacy.Name != "" {
		t.Errorf("NULL login_name/name must fall back to mapped uid/empty: %+v", legacy)
	}
	if legacy.CreatedAt.UnixMilli() != 0 {
		t.Errorf("NULL last_activity must map to created_at 0, got %d", legacy.CreatedAt.UnixMilli())
	}

	// Sessions (type 0) and wipe tokens (type 2) are not imported.
	for _, hash := range []string{"session-hash-value", "wipe-hash-value", "ghost-hash-value", "nouser-hash-value"} {
		if _, err := as.GetByHash(ctx, hash); !errors.Is(err, auth.ErrTokenNotFound) {
			t.Errorf("hash %q must not be imported: %v", hash, err)
		}
	}
	if n := countRows(t, cfgPath, "app_passwords"); n != 2 {
		t.Errorf("app_passwords count = %d, want 2", n)
	}

	// Second run: fully idempotent, imported rows skip on their hash.
	out, err = runCLI(t, "", importTokensArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "app passwords: 0 created, 5 skipped (existing), 0 failed") {
		t.Errorf("second run output:\n%s", out)
	}
	if n := countRows(t, cfgPath, "app_passwords"); n != 2 {
		t.Errorf("app_passwords count after re-run = %d, want 2", n)
	}
}

func TestImportNCTokensDryRun(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importTokensSourceEnv(t, "nc_")
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "legacyuser")

	out, err := runCLI(t, "", importTokensArgs(cfgPath, dsn, "--table-prefix", "nc_", "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"app passwords: 2 created, 3 skipped (existing), 0 failed",
		"dry-run: no changes written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "app_passwords"); n != 0 {
		t.Errorf("dry-run must not write app passwords, got %d", n)
	}
}

func TestImportNCTokensMissingTable(t *testing.T) {
	cfgPath := cliEnv(t)
	// A source holding only oc_users (no authtoken table) is a hard error,
	// like the users importer's required tables.
	dsn := func() string {
		dir := t.TempDir()
		dsn := "file:" + filepath.Join(dir, "source.db")
		ctx := context.Background()
		db, err := database.Open(ctx, database.Config{Driver: database.DialectSQLite, DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(ctx, `CREATE TABLE oc_users (uid TEXT PRIMARY KEY, uid_lower TEXT)`); err != nil {
			t.Fatal(err)
		}
		return dsn
	}()

	_, err := runCLI(t, "", importTokensArgs(cfgPath, dsn)...)
	if err == nil || !strings.Contains(err.Error(), "read source authtokens") {
		t.Errorf("missing authtoken table must fail the run, got %v", err)
	}
}
