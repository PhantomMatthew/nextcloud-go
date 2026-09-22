package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// importSharesEdgeEnv creates a minimal source database exercising the
// remaining skip branches: unknown recipient group, empty recipient,
// unparseable expiration, token-less link, and a filecache path outside the
// user's files root.
func importSharesEdgeEnv(t *testing.T, prefix string) string {
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
	if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`storages (numeric_id, id) VALUES (1, 'home::alice')`); err != nil {
		t.Fatal(err)
	}
	for _, f := range [][2]any{
		{10, "files/Documents/report.pdf"},
		{11, "files_trashbin/deleted.txt"},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`filecache (fileid, storage, path) VALUES (?, 1, ?)`,
			f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) *string { return &s }
	rows := []struct {
		shareType  int
		shareWith  *string
		fileSource int64
		expiration *string
		token      *string
	}{
		{1, str("nogroup"), 10, nil, nil},              // unknown recipient group
		{0, nil, 10, nil, nil},                         // empty recipient
		{3, nil, 10, str("garbage"), str("tokBadExp")}, // unparseable expiration
		{3, nil, 10, nil, nil},                         // link without token
		{0, str("bob"), 11, nil, nil},                  // path outside files/ root
		{3, nil, 10, nil, str("tokGood")},              // created
	}
	for i, sh := range rows {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`share
			(id, share_type, share_with, uid_owner, uid_initiator, item_type, file_source, file_target,
			 permissions, stime, expiration, token, password, label, note)
			VALUES (?, ?, ?, 'alice', 'alice', 'file', ?, '/ignored', 1, 1600000000, ?, ?, ?, ?, ?)`,
			i+1, sh.shareType, sh.shareWith, sh.fileSource, sh.expiration, sh.token, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func TestImportNCSharesEdge(t *testing.T) {
	cfgPath, _ := cliFilesEnv(t)
	dsn := importSharesEdgeEnv(t, "oc_")
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")
	createAliceFiles(t, cfgPath)

	out, err := runCLI(t, "", importSharesArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"user shares: 0 created, 2 skipped (existing), 0 failed",
		"group shares: 0 created, 1 skipped (existing), 0 failed",
		"link shares: 1 created, 2 skipped (existing), 0 failed",
		"warning: share 1: recipient group nogroup not in target, skipped",
		"warning: share 2: empty recipient, skipped",
		`warning: share 3: unparseable expiration "garbage", skipped`,
		"warning: share 4: link share without token, skipped",
		`warning: share 5: filecache path "files_trashbin/deleted.txt" is outside the user's files root, skipped`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "shares"); n != 1 {
		t.Errorf("share count = %d, want 1", n)
	}
}

func TestImportNCSharesTokenRetry(t *testing.T) {
	cfgPath, _ := cliFilesEnv(t)
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")
	createAliceFiles(t, cfgPath)

	// Force the generated user/group-share token to collide once with an
	// existing share's token; the insert must be retried with a fresh token.
	ctx := context.Background()
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	us := users.NewSQLStore(db)
	fs := files.NewSQLStore(db)
	ss := sharing.NewSQLShareStore(db)
	alice, err := us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.Insert(ctx, &files.Share{
		OwnerUserID: alice.ID,
		ShareType:   files.ShareTypeLink,
		Path:        "/a.txt",
		ItemType:    "file",
		Token:       "collide1",
		Permissions: 1,
		StimeMs:     1,
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	orig := newImportToken
	newImportToken = func() (string, error) {
		calls++
		if calls == 1 {
			return "collide1", nil
		}
		return orig()
	}
	t.Cleanup(func() { newImportToken = orig })

	r := newImportReport()
	row := &ncShareRow{
		id: 99, shareType: ncShareTypeUser, shareWith: "bob", uidOwner: "alice",
		fileSource: 10, permissions: 1, stime: 1, hasFC: true, fcPath: "files/Documents/report.pdf", storageID: "home::alice",
	}
	notes := 0
	importNCShare(ctx, row, us, fs, ss, false, r, &notes)
	if calls != 2 {
		t.Errorf("token generator calls = %d, want 2 (collision + retry)", calls)
	}
	uc := r.counts["user shares"]
	if uc == nil || uc.created != 1 || uc.failed != 0 {
		t.Errorf("counts = %+v, want 1 created", uc)
	}
	existing, err := ss.GetByToken(ctx, "collide1")
	if err != nil {
		t.Fatal(err)
	}
	if existing.ShareType != files.ShareTypeLink {
		t.Errorf("pre-existing share must be untouched: %+v", existing)
	}
	items, err := ss.ListByOwner(ctx, alice.ID, "/Documents/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	imported := findShare(t, items, ncShareTypeUser, "bob")
	if imported.Token == "collide1" || len(imported.Token) != 15 {
		t.Errorf("imported share token = %q, want a fresh 15-char token", imported.Token)
	}
}
