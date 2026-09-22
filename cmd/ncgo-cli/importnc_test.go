package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// importSourceEnv creates a temp sqlite database holding a minimal PHP
// Nextcloud schema (<prefix>users, <prefix>groups, <prefix>group_user) with
// fixture rows, returning the DSN and the argon2id hashes used per source uid.
func importSourceEnv(t *testing.T, prefix string) (string, map[string]string) {
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
		`CREATE TABLE ` + prefix + `groups (gid TEXT PRIMARY KEY)`,
		`CREATE TABLE ` + prefix + `group_user (gid TEXT, uid TEXT, PRIMARY KEY (gid, uid))`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := auth.NewArgon2id(auth.Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
	hash := func(pw string) string {
		s, err := h.Hash(pw)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	hashes := map[string]string{
		"alice":       hash("alicepw"),
		"Legacy User": hash("legacypw"),
	}
	type userRow struct {
		uid, uidLower string
		display       *string
		password      *string
	}
	str := func(s string) *string { return &s }
	usersRows := []userRow{
		{"alice", "alice", str("Alice A"), str(hashes["alice"])},
		{"bob", "bob", str("Bob B"), str("$2y$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")},
		{"bad uid", "bad uid", str("Bad"), str("")},
		{"Legacy User", "legacyuser", str("Legacy"), str(hashes["Legacy User"])},
		{"nodisplay", "nodisplay", nil, str(hash("nodisplaypw"))},
	}
	for _, u := range usersRows {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`users (uid, uid_lower, displayname, password) VALUES (?, ?, ?, ?)`,
			u.uid, u.uidLower, u.display, u.password); err != nil {
			t.Fatal(err)
		}
	}
	for _, gid := range []string{"team", "ops"} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`groups (gid) VALUES (?)`, gid); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range [][2]string{
		{"team", "alice"},
		{"team", "bob"},
		{"ops", "Legacy User"},
		{"team", "ghost"},    // user not in oc_users
		{"nogroup", "alice"}, // group not in oc_groups
	} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`group_user (gid, uid) VALUES (?, ?)`, m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	return dsn, hashes
}

func openTargetStore(t *testing.T, cfgPath string) *users.SQLStore {
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
	return users.NewSQLStore(db)
}

func importArgs(cfgPath, dsn string, extra ...string) []string {
	args := make([]string, 0, 8+len(extra))
	args = append(args,
		"--config", cfgPath, "import-nextcloud", "users",
		"--source-driver", "sqlite", "--source-dsn", dsn,
	)
	return append(args, extra...)
}

func TestImportNCUsers(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn, hashes := importSourceEnv(t, "oc_")

	out, err := runCLI(t, "", importArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"users: 4 created, 1 skipped (existing), 0 failed",
		"groups: 2 created, 0 skipped (existing), 0 failed",
		"memberships: 3 created, 2 skipped (existing), 0 failed",
		"warning: user bob: password not imported (unsupported hash), must reset",
		"warning: user bad uid: uid not valid for ncgo, skipped",
		"warning: user Legacy User: uid not valid for ncgo, imported as legacyuser",
		"warning: membership team/ghost: user not imported, skipped",
		"warning: membership nogroup/alice: group not imported, skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	ctx := context.Background()
	store := openTargetStore(t, cfgPath)

	alice, err := store.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice.PasswordHash != hashes["alice"] {
		t.Error("argon2id hash must be imported byte-identical")
	}
	if alice.DisplayName != "Alice A" || !alice.Enabled {
		t.Errorf("alice = %+v", alice)
	}
	bob, err := store.GetByUID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.PasswordHash != noPasswordSentinel {
		t.Errorf("bcrypt user must get the sentinel, got %q", bob.PasswordHash)
	}
	legacy, err := store.GetByUID(ctx, "legacyuser")
	if err != nil {
		t.Fatal("uid_lower fallback user must exist")
	}
	if legacy.DisplayName != "Legacy" {
		t.Errorf("legacy display = %q", legacy.DisplayName)
	}
	nod, err := store.GetByUID(ctx, "nodisplay")
	if err != nil {
		t.Fatal(err)
	}
	if nod.DisplayName != "nodisplay" {
		t.Errorf("displayname fallback = %q", nod.DisplayName)
	}
	if _, err := store.GetByUID(ctx, "bad uid"); !errors.Is(err, users.ErrNotFound) {
		t.Errorf("invalid uid must not be imported: %v", err)
	}
	for _, gid := range []string{"team", "ops"} {
		if _, err := store.GetGroupByGID(ctx, gid); err != nil {
			t.Errorf("group %s: %v", gid, err)
		}
	}
	team, err := store.GroupMembers(ctx, "team", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(team) != "[alice bob]" {
		t.Errorf("team members = %v", team)
	}
	ops, err := store.GroupMembers(ctx, "ops", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ops) != "[legacyuser]" {
		t.Errorf("ops members = %v", ops)
	}

	// The imported argon2id hash must verify through ncgo's verifier,
	// proving PHP password_hash PHC compatibility.
	h := auth.NewArgon2id(auth.Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
	v := users.NewPasswordVerifier(store, h)
	if _, err := v.Verify(ctx, "alice", "alicepw"); err != nil {
		t.Errorf("imported argon2id user does not verify: %v", err)
	}
	if _, err := v.Verify(ctx, "legacyuser", "legacypw"); err != nil {
		t.Errorf("remapped user does not verify: %v", err)
	}
	if _, err := v.Verify(ctx, "bob", "password"); err == nil {
		t.Error("sentinel password must never verify")
	}

	// Second run: fully idempotent, everything skipped.
	out, err = runCLI(t, "", importArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"users: 0 created, 5 skipped (existing), 0 failed",
		"groups: 0 created, 2 skipped (existing), 0 failed",
		"memberships: 0 created, 5 skipped (existing), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output missing %q:\n%s", want, out)
		}
	}
	n, err := store.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("user count after re-run = %d, want 4", n)
	}
}

func TestImportNCUsersDryRun(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn, _ := importSourceEnv(t, "nc_")

	out, err := runCLI(t, "", importArgs(cfgPath, dsn, "--table-prefix", "nc_", "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"users: 4 created, 1 skipped (existing), 0 failed",
		"groups: 2 created, 0 skipped (existing), 0 failed",
		"memberships: 3 created, 2 skipped (existing), 0 failed",
		"dry-run: no changes written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	ctx := context.Background()
	store := openTargetStore(t, cfgPath)
	n, err := store.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("dry-run must not write users, got %d", n)
	}
	gs, err := store.ListGroups(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 0 {
		t.Errorf("dry-run must not write groups, got %d", len(gs))
	}
}

func TestImportNCUsersExistingTargetUser(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn, _ := importSourceEnv(t, "oc_")

	ctx := context.Background()
	store := openTargetStore(t, cfgPath)
	if err := store.Create(ctx, &users.User{
		UID: "alice", DisplayName: "Pre-existing", PasswordHash: "originalhash", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", importArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "users: 3 created, 2 skipped (existing), 0 failed") {
		t.Errorf("output:\n%s", out)
	}
	alice, err := store.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice.DisplayName != "Pre-existing" || alice.PasswordHash != "originalhash" {
		t.Errorf("pre-existing user must not be overwritten: %+v", alice)
	}
	// The pre-existing user still receives her memberships.
	team, err := store.GroupMembers(ctx, "team", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(team) != "[alice bob]" {
		t.Errorf("team members = %v", team)
	}
}

func TestImportNCFlagValidation(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn, _ := importSourceEnv(t, "oc_")

	if _, err := runCLI(t, "", "--config", cfgPath, "import-nextcloud", "users",
		"--source-driver", "oracle", "--source-dsn", dsn); err == nil ||
		!strings.Contains(err.Error(), "--source-driver") {
		t.Errorf("bad driver = %v", err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "import-nextcloud", "users",
		"--source-driver", "sqlite"); err == nil ||
		!strings.Contains(err.Error(), "--source-dsn is required") {
		t.Errorf("missing dsn = %v", err)
	}
	if _, err := runCLI(t, "", importArgs(cfgPath, dsn, "--table-prefix", "oc_;DROP")...); err == nil ||
		!strings.Contains(err.Error(), "--table-prefix") {
		t.Errorf("bad prefix = %v", err)
	}
	if _, err := runCLI(t, "", importArgs(cfgPath, "file:"+filepath.Join(os.TempDir(), "no-such-dir", "x.db"))...); err == nil {
		t.Error("unreachable source must fail")
	}
}
