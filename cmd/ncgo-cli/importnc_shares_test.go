package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// importSharesSourceEnv creates a temp sqlite database holding the PHP
// Nextcloud share tables (<prefix>share, <prefix>filecache, <prefix>storages)
// with one fixture row per supported and skipped category, returning the DSN
// and the argon2id hash used for the protected link.
func importSharesSourceEnv(t *testing.T, prefix string) (dsn, pwHash string) {
	t.Helper()
	dir := t.TempDir()
	dsn = "file:" + filepath.Join(dir, "source.db")
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{Driver: database.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TABLE ` + prefix + `share (id INTEGER PRIMARY KEY, share_type INTEGER, share_with TEXT,
			uid_owner TEXT, uid_initiator TEXT, item_type TEXT, file_source INTEGER, file_target TEXT,
			permissions INTEGER, stime INTEGER, expiration TEXT, token TEXT, password TEXT, label TEXT, note TEXT)`,
		`CREATE TABLE ` + prefix + `filecache (fileid INTEGER PRIMARY KEY, storage INTEGER, path TEXT)`,
		`CREATE TABLE ` + prefix + `storages (numeric_id INTEGER PRIMARY KEY, id TEXT)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := auth.NewArgon2id(auth.Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
	pwHash, err = h.Hash("linksecret")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		numericID int64
		id        string
	}{
		{1, "home::alice"},
		{2, "home::bob"},
		{3, "local::/mnt/external/"},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`storages (numeric_id, id) VALUES (?, ?)`,
			s.numericID, s.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct {
		fileid  int64
		storage int64
		path    string
	}{
		{10, 1, "files/Documents/report.pdf"},
		{11, 1, "files/Photos"},
		{12, 3, "files/ext.txt"},
		{13, 1, "files/missing.txt"},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`filecache (fileid, storage, path) VALUES (?, ?, ?)`,
			f.fileid, f.storage, f.path); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) *string { return &s }
	type shareRow struct {
		shareType  int
		shareWith  *string
		owner      string
		fileSource int64
		perms      int
		stime      int64
		expiration *string
		token      *string
		password   *string
		label      *string
		note       *string
	}
	rows := []shareRow{
		// 1: user share alice→bob (created), carries a note (dropped).
		{0, str("bob"), "alice", 10, 17, 1600000000, nil, nil, nil, nil, str("remember this")},
		// 2: group share alice→team on a folder (created).
		{1, str("team"), "alice", 11, 31, 1600000001, nil, nil, nil, nil, nil},
		// 3: protected link (created): argon2id hash, label, expiration.
		{3, nil, "alice", 10, 1, 1600000002, str("2030-01-02 03:04:05"), str("tokPW12345"), str(pwHash), str("pw link"), nil},
		// 4: open link (created).
		{3, nil, "alice", 11, 1, 1600000003, nil, str("tokOpen123"), nil, nil, nil},
		// 5: bcrypt-protected link (skipped — never import unprotected).
		{3, nil, "alice", 10, 1, 1600000004, nil, str("tokBcrypt"), str("$2y$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"), nil, nil},
		// 6, 7: remote/federated (skipped).
		{4, str("bob@remote.example"), "alice", 10, 1, 1600000005, nil, str("tokRemote1"), nil, nil, nil},
		{6, str("carol@other.example"), "alice", 10, 1, 1600000006, nil, str("tokRemote2"), nil, nil, nil},
		// 8: no filecache row (skipped).
		{0, str("bob"), "alice", 99, 1, 1600000007, nil, nil, nil, nil, nil},
		// 9: external storage (skipped).
		{0, str("bob"), "alice", 12, 1, 1600000008, nil, nil, nil, nil, nil},
		// 10: recipient user not in target (skipped).
		{0, str("ghost"), "alice", 10, 1, 1600000009, nil, nil, nil, nil, nil},
		// 11: owner not in target (skipped).
		{0, str("bob"), "mallory", 10, 1, 1600000010, nil, nil, nil, nil, nil},
		// 12: shared file not in the target filecache (skipped).
		{0, str("bob"), "alice", 13, 1, 1600000011, nil, nil, nil, nil, nil},
	}
	for i, sh := range rows {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`share
			(id, share_type, share_with, uid_owner, uid_initiator, item_type, file_source, file_target,
			 permissions, stime, expiration, token, password, label, note)
			VALUES (?, ?, ?, ?, ?, 'file', ?, '/ignored', ?, ?, ?, ?, ?, ?, ?)`,
			i+1, sh.shareType, sh.shareWith, sh.owner, sh.owner, sh.fileSource,
			sh.perms, sh.stime, sh.expiration, sh.token, sh.password, sh.label, sh.note); err != nil {
			t.Fatal(err)
		}
	}
	return dsn, pwHash
}

func importSharesArgs(cfgPath, dsn string, extra ...string) []string {
	args := make([]string, 0, 8+len(extra))
	args = append(args,
		"--config", cfgPath, "import-nextcloud", "shares",
		"--source-driver", "sqlite", "--source-dsn", dsn,
	)
	return append(args, extra...)
}

// openTargetShareStore builds the sharing store against the target config for
// post-import assertions.
func openTargetShareStore(t *testing.T, cfgPath string) *sharing.SQLShareStore {
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
	return sharing.NewSQLShareStore(db)
}

// createAliceFiles populates alice's target filecache through the DAV the way
// the files import (4e2) would have.
func createAliceFiles(t *testing.T, cfgPath string) {
	t.Helper()
	ctx := context.Background()
	dav := openTargetDAV(t, cfgPath)
	if _, err := dav.Mkdir(ctx, "alice", "/Documents"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/Documents/report.pdf", strings.NewReader("pdf"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Mkdir(ctx, "alice", "/Photos"); err != nil {
		t.Fatal(err)
	}
}

func findShare(t *testing.T, items []files.Share, shareType int, shareWith string) *files.Share {
	t.Helper()
	for i := range items {
		if items[i].ShareType == shareType && items[i].ShareWith == shareWith {
			return &items[i]
		}
	}
	t.Fatalf("share type %d with %q not found in %v", shareType, shareWith, items)
	return nil
}

func TestImportNCShares(t *testing.T) {
	cfgPath, _ := cliFilesEnv(t)
	dsn, pwHash := importSharesSourceEnv(t, "oc_")
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")
	store := openTargetStore(t, cfgPath)
	if err := store.CreateGroup(context.Background(), &users.Group{GID: "team"}); err != nil {
		t.Fatal(err)
	}
	createAliceFiles(t, cfgPath)

	out, err := runCLI(t, "", importSharesArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"user shares: 1 created, 5 skipped (existing), 0 failed",
		"group shares: 1 created, 0 skipped (existing), 0 failed",
		"link shares: 2 created, 1 skipped (existing), 0 failed",
		"remote shares: 0 created, 2 skipped (existing), 0 failed",
		"warning: share 5: link password not imported (unsupported hash format), share skipped",
		"warning: share 6: type 4 (remote/federated/circle) not supported, skipped",
		"warning: share 7: type 6 (remote/federated/circle) not supported, skipped",
		"warning: share 8: no filecache row for fileid 99, skipped",
		`warning: share 9: fileid 12 on non-home storage "local::/mnt/external/" (external storage unsupported), skipped`,
		"warning: share 10: recipient user ghost not in target, skipped",
		"warning: share 11: owner mallory not in target (run 'import-nextcloud users' first), skipped",
		"warning: share 12: /missing.txt not in target filecache for alice (run 'import-nextcloud files' first), skipped",
		"warning: 1 share note(s) not imported (ncgo has no note field)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	ctx := context.Background()
	us := openTargetStore(t, cfgPath)
	ss := openTargetShareStore(t, cfgPath)
	alice, err := us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	// The user share lands with ncgo semantics: generated 15-char token,
	// verbatim permission bitmask, stime seconds → ms.
	docs, err := ss.ListByOwner(ctx, alice.ID, "/Documents/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	userShare := findShare(t, docs, 0, "bob")
	if userShare.Permissions != 17 {
		t.Errorf("user share permissions = %d, want 17 (verbatim)", userShare.Permissions)
	}
	if userShare.StimeMs != 1600000000000 {
		t.Errorf("user share stime_ms = %d, want 1600000000000", userShare.StimeMs)
	}
	if len(userShare.Token) != 15 {
		t.Errorf("user share token = %q, want a generated 15-char token", userShare.Token)
	}
	if userShare.Accepted != 1 || userShare.ItemType != "file" || userShare.PasswordHash != "" {
		t.Errorf("user share = %+v", userShare)
	}

	photos, err := ss.ListByOwner(ctx, alice.ID, "/Photos")
	if err != nil {
		t.Fatal(err)
	}
	groupShare := findShare(t, photos, 1, "team")
	if groupShare.Permissions != 31 || groupShare.ItemType != "folder" {
		t.Errorf("group share = %+v", groupShare)
	}

	// The group share is visible to sharees through the store's read path.
	incoming, err := ss.ListBySharee(ctx, "", []string{"team"})
	if err != nil {
		t.Fatal(err)
	}
	if len(incoming) != 1 || incoming[0].Path != "/Photos" {
		t.Errorf("ListBySharee(team) = %+v", incoming)
	}

	// Link tokens are imported verbatim; the argon2id password hash is
	// byte-identical and verifies through ncgo's public-link password check
	// (PHP password_hash PHC compatibility, as for users in 4e1).
	pwShare, err := ss.GetByToken(ctx, "tokPW12345")
	if err != nil {
		t.Fatal(err)
	}
	if pwShare.PasswordHash != pwHash {
		t.Error("argon2id link hash must be imported byte-identical")
	}
	if pwShare.Label != "pw link" {
		t.Errorf("link label = %q", pwShare.Label)
	}
	wantExpire := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli()
	if pwShare.ExpireMs != wantExpire {
		t.Errorf("link expire_ms = %d, want %d", pwShare.ExpireMs, wantExpire)
	}
	openShare, err := ss.GetByToken(ctx, "tokOpen123")
	if err != nil {
		t.Fatal(err)
	}
	if openShare.PasswordHash != "" || openShare.ExpireMs != 0 {
		t.Errorf("open link = %+v", openShare)
	}
	svc := &sharing.Service{Store: ss, Users: us, Hasher: auth.NewArgon2id(auth.Argon2idParams{
		MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16,
	})}
	if _, _, err := svc.ResolvePublic(ctx, "tokPW12345", "linksecret"); err != nil {
		t.Errorf("imported link password does not verify: %v", err)
	}
	if _, _, err := svc.ResolvePublic(ctx, "tokPW12345", "wrong"); err == nil {
		t.Error("wrong password must not verify")
	}
	if _, _, err := svc.ResolvePublic(ctx, "tokOpen123", ""); err != nil {
		t.Errorf("open link must resolve without a password: %v", err)
	}
	if _, err := ss.GetByToken(ctx, "tokBcrypt"); err == nil {
		t.Error("bcrypt-protected link must not be imported")
	}

	// Second run: fully idempotent, everything skipped.
	out, err = runCLI(t, "", importSharesArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"user shares: 0 created, 6 skipped (existing), 0 failed",
		"group shares: 0 created, 1 skipped (existing), 0 failed",
		"link shares: 0 created, 3 skipped (existing), 0 failed",
		"remote shares: 0 created, 2 skipped (existing), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "shares"); n != 4 {
		t.Errorf("share count after re-run = %d, want 4", n)
	}
}

func TestImportNCSharesDryRun(t *testing.T) {
	cfgPath, _ := cliFilesEnv(t)
	dsn, _ := importSharesSourceEnv(t, "nc_")
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")
	store := openTargetStore(t, cfgPath)
	if err := store.CreateGroup(context.Background(), &users.Group{GID: "team"}); err != nil {
		t.Fatal(err)
	}
	createAliceFiles(t, cfgPath)

	out, err := runCLI(t, "", importSharesArgs(cfgPath, dsn, "--table-prefix", "nc_", "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"user shares: 1 created, 5 skipped (existing), 0 failed",
		"group shares: 1 created, 0 skipped (existing), 0 failed",
		"link shares: 2 created, 1 skipped (existing), 0 failed",
		"dry-run: no changes written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "shares"); n != 0 {
		t.Errorf("dry-run must not write shares, got %d", n)
	}
}

func TestImportNCSharesTokenCollision(t *testing.T) {
	cfgPath, _ := cliFilesEnv(t)
	dsn, _ := importSharesSourceEnv(t, "oc_")
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")
	store := openTargetStore(t, cfgPath)
	if err := store.CreateGroup(context.Background(), &users.Group{GID: "team"}); err != nil {
		t.Fatal(err)
	}
	createAliceFiles(t, cfgPath)

	// An unrelated share already holds the fixture's open-link token.
	ctx := context.Background()
	bob, err := store.GetByUID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	ss := openTargetShareStore(t, cfgPath)
	existing := &files.Share{
		OwnerUserID: bob.ID,
		ShareType:   files.ShareTypeLink,
		Path:        "/other.txt",
		ItemType:    "file",
		Token:       "tokOpen123",
		Permissions: 1,
		StimeMs:     1,
	}
	if err := ss.Insert(ctx, existing); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", importSharesArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: share 4: token tokOpen123 already used by an unrelated share, skipped") {
		t.Errorf("output missing collision warning:\n%s", out)
	}
	if !strings.Contains(out, "link shares: 1 created, 2 skipped (existing), 0 failed") {
		t.Errorf("output:\n%s", out)
	}
	// The pre-existing share is untouched.
	held, err := ss.GetByToken(ctx, "tokOpen123")
	if err != nil {
		t.Fatal(err)
	}
	if held.OwnerUserID != bob.ID || held.Path != "/other.txt" {
		t.Errorf("pre-existing share must not be overwritten: %+v", held)
	}
}

func TestNCHomePath(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"files", "/", true},
		{"files/a/b.txt", "/a/b.txt", true},
		{"files_trashbin/x", "", false},
		{"files/", "", false},
		{"other", "", false},
	} {
		got, ok := ncHomePath(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ncHomePath(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestNCExpirationMs(t *testing.T) {
	if ms, err := ncExpirationMs(""); err != nil || ms != 0 {
		t.Errorf("empty = %d, %v", ms, err)
	}
	if ms, err := ncExpirationMs("  "); err != nil || ms != 0 {
		t.Errorf("blank = %d, %v", ms, err)
	}
	want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli()
	if ms, err := ncExpirationMs("2030-01-02 03:04:05"); err != nil || ms != want {
		t.Errorf("datetime = %d, %v; want %d", ms, err, want)
	}
	if _, err := ncExpirationMs("not a date"); err == nil {
		t.Error("garbage must error")
	}
	if _, err := ncExpirationMs("2030-01-02"); err == nil {
		t.Error("date-only must error (skip, never guess the expiry)")
	}
}
