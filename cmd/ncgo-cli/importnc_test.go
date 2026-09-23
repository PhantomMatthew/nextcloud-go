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
		`CREATE TABLE ` + prefix + `preferences (userid TEXT, appid TEXT, configkey TEXT, configvalue TEXT,
			PRIMARY KEY (userid, appid, configkey))`,
		`CREATE TABLE ` + prefix + `accounts (uid TEXT PRIMARY KEY, data TEXT)`,
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
	// Email/quota fixtures: alice gets a settings email + a 5 GB quota, bob an
	// explicit "none" quota, nodisplay an accounts-only email.
	for _, p := range [][4]string{
		{"alice", "settings", "email", "alice@example.com"},
		{"alice", "files", "quota", "5 GB"},
		{"bob", "files", "quota", "none"},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`preferences (userid, appid, configkey, configvalue) VALUES (?, ?, ?, ?)`,
			p[0], p[1], p[2], p[3]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`accounts (uid, data) VALUES (?, ?)`,
		"nodisplay", `{"displayname": {"value": "No Display"}, "email": {"value": "nodisplay@example.com", "scope": "v2-federated", "verified": "0"}}`); err != nil {
		t.Fatal(err)
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
	if alice.Email != "alice@example.com" {
		t.Errorf("alice email = %q, want settings/email", alice.Email)
	}
	if alice.QuotaBytes == nil || *alice.QuotaBytes != 5<<30 {
		t.Errorf("alice quota = %v, want 5 GB in bytes", alice.QuotaBytes)
	}
	bob, err := store.GetByUID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.PasswordHash != noPasswordSentinel {
		t.Errorf("bcrypt user must get the sentinel, got %q", bob.PasswordHash)
	}
	if bob.QuotaBytes != nil || bob.Email != "" {
		t.Errorf("bob quota none must map to NULL, empty email: %+v", bob)
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
	if nod.Email != "nodisplay@example.com" {
		t.Errorf("nodisplay email = %q, want the oc_accounts JSON email", nod.Email)
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

// importExtrasSourceEnv builds a source whose users cover the quota states
// (none/default/human size/zero/invalid/bare bytes) and every email source
// (primary_email precedence, legacy settings email, accounts-only, broken
// accounts JSON, unset).
func importExtrasSourceEnv(t *testing.T, prefix string) string {
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
		`CREATE TABLE ` + prefix + `preferences (userid TEXT, appid TEXT, configkey TEXT, configvalue TEXT,
			PRIMARY KEY (userid, appid, configkey))`,
		`CREATE TABLE ` + prefix + `accounts (uid TEXT PRIMARY KEY, data TEXT)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := auth.NewArgon2id(auth.Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
	hash, err := h.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	uids := []string{
		"q-none", "q-default", "q-5gb", "q-512mb", "q-zero", "q-bare", "q-bad",
		"e-primary", "e-legacy", "e-accounts", "e-broken", "e-none",
	}
	for _, uid := range uids {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`users (uid, uid_lower, displayname, password) VALUES (?, ?, NULL, ?)`,
			uid, uid, hash); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range [][4]string{
		{"q-none", "files", "quota", "none"},
		{"q-default", "files", "quota", "default"},
		{"q-5gb", "files", "quota", "5 GB"},
		{"q-512mb", "files", "quota", "512 MB"},
		{"q-zero", "files", "quota", "0"},
		{"q-bare", "files", "quota", "1073741824"},
		{"q-bad", "files", "quota", "plenty"},
		{"e-primary", "settings", "email", "legacy@example.com"},
		{"e-primary", "settings", "primary_email", "primary@example.com"},
		{"e-legacy", "settings", "email", "legacy@example.com"},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`preferences (userid, appid, configkey, configvalue) VALUES (?, ?, ?, ?)`,
			p[0], p[1], p[2], p[3]); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range [][2]string{
		{"e-accounts", `{"email": {"value": "accounts@example.com", "scope": "v2-local", "verified": "0"}}`},
		{"e-broken", `{not json`},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`accounts (uid, data) VALUES (?, ?)`, a[0], a[1]); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func TestImportNCUsersEmailQuota(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importExtrasSourceEnv(t, "oc_")

	out, err := runCLI(t, "", importArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"users: 12 created, 0 skipped (existing), 0 failed",
		`warning: user q-bad: quota "plenty" not understood, imported without quota`,
		"warning: user e-broken: accounts data not parseable, email not imported from it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	ctx := context.Background()
	store := openTargetStore(t, cfgPath)
	quota := func(uid string) *int64 {
		t.Helper()
		u, err := store.GetByUID(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		return u.QuotaBytes
	}
	gb := int64(1 << 30)
	for _, tt := range []struct {
		uid  string
		want *int64
	}{
		{"q-none", nil},
		{"q-default", nil},
		{"q-5gb", ptr(5 * gb)},
		{"q-512mb", ptr(512 << 20)},
		{"q-zero", ptr(0)},
		{"q-bare", ptr(gb)},
		{"q-bad", nil},
	} {
		got := quota(tt.uid)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("%s quota = %v, want %v", tt.uid, got, tt.want)
		}
	}
	email := func(uid string) string {
		t.Helper()
		u, err := store.GetByUID(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		return u.Email
	}
	for uid, want := range map[string]string{
		"e-primary":  "primary@example.com", // primary_email beats settings/email
		"e-legacy":   "legacy@example.com",
		"e-accounts": "accounts@example.com",
		"e-broken":   "",
		"e-none":     "",
		"q-5gb":      "",
	} {
		if got := email(uid); got != want {
			t.Errorf("%s email = %q, want %q", uid, got, want)
		}
	}

	// Re-run: idempotent, nobody is updated and no values change.
	out, err = runCLI(t, "", importArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "users: 0 created, 12 skipped (existing), 0 failed") {
		t.Errorf("second run output:\n%s", out)
	}
	if got := email("e-primary"); got != "primary@example.com" {
		t.Errorf("e-primary email after re-run = %q", got)
	}
}

func TestImportNCUsersEmailQuotaDryRun(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importExtrasSourceEnv(t, "nc_")

	out, err := runCLI(t, "", importArgs(cfgPath, dsn, "--table-prefix", "nc_", "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"users: 12 created, 0 skipped (existing), 0 failed",
		`warning: user q-bad: quota "plenty" not understood, imported without quota`,
		"dry-run: no changes written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "users"); n != 0 {
		t.Errorf("dry-run must not write users, got %d", n)
	}
}

func ptr(v int64) *int64 { return &v }

func TestParseNCQuota(t *testing.T) {
	gb := int64(1 << 30)
	for _, tt := range []struct {
		raw  string
		want *int64
		ok   bool
	}{
		{"", nil, true},
		{"none", nil, true},
		{"default", nil, true},
		{" None ", nil, true},
		{"5 GB", ptr(5 * gb), true},
		{"5GB", ptr(5 * gb), true},
		{"512 MB", ptr(512 << 20), true},
		{"1.5 GB", ptr(3 * gb / 2), true},
		{"2 TB", ptr(2 << 40), true},
		{"0", ptr(0), true},
		{"0 B", ptr(0), true},
		{"1073741824", ptr(gb), true},
		{"1024", ptr(1024), true},
		{"plenty", nil, false},
		{"-5 GB", nil, false},
		{"GB", nil, false},
		{"5 XB", nil, false},
		{"", nil, true},
	} {
		got, ok := parseNCQuota(tt.raw)
		if ok != tt.ok {
			t.Errorf("parseNCQuota(%q) ok = %v, want %v", tt.raw, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("parseNCQuota(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestNCAccountEmail(t *testing.T) {
	email, err := ncAccountEmail(`{"email": {"value": "a@example.com", "scope": "v2-federated", "verified": "0"}, "displayname": {"value": "A"}}`)
	if err != nil || email != "a@example.com" {
		t.Errorf("email = %q, %v", email, err)
	}
	email, err = ncAccountEmail(`{"displayname": {"value": "A"}}`)
	if err != nil || email != "" {
		t.Errorf("missing email property = %q, %v", email, err)
	}
	if _, err = ncAccountEmail(`{broken`); err == nil {
		t.Error("malformed JSON must error")
	}
	if _, err = ncAccountEmail(`{"email": "a@example.com"}`); err == nil {
		t.Error("non-object email property must error")
	}
}
