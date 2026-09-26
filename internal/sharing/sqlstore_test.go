package sharing

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

func testDB(t *testing.T) database.DB {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedUser(t *testing.T, db database.DB) int64 {
	t.Helper()
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := users.NewSQLStore(db).Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestSQLShareStoreCRUD(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	sh := &files.Share{
		OwnerUserID: uid,
		ShareType:   files.ShareTypeLink,
		Path:        "/a.txt",
		ItemType:    "file",
		Token:       "ncgopublic00001",
		Permissions: 1,
		StimeMs:     1746100800000,
	}
	if err := store.Insert(ctx, sh); err != nil {
		t.Fatal(err)
	}
	if sh.ID == 0 {
		t.Fatal("id not assigned")
	}
	got, err := store.GetByToken(ctx, "ncgopublic00001")
	if err != nil || got.Path != "/a.txt" {
		t.Fatalf("get token = %+v err=%v", got, err)
	}
	dup := *sh
	dup.ID = 0
	dup.Token = "ncgopublic00001"
	if err := store.Insert(ctx, &dup); !errors.Is(err, files.ErrExists) {
		t.Fatalf("unique token = %v", err)
	}
	listed, err := store.ListByOwner(ctx, uid, "/a.txt")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v %v", listed, err)
	}
	if err := store.RenamePath(ctx, uid, "/a.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByToken(ctx, "ncgopublic00001"); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetByID(ctx, sh.ID)
	if err != nil || got.Path != "/b.txt" {
		t.Fatalf("renamed = %+v err=%v", got, err)
	}
	if err := store.DeleteByPath(ctx, uid, "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByID(ctx, sh.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
}

func TestSQLShareStoreListBySharee(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	userShare := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeUser, Path: "/a.txt",
		ItemType: "file", Token: "user00000000001", Permissions: 1, ShareWith: "bob", StimeMs: 1746100800000,
	}
	groupShare := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeGroup, Path: "/pub",
		ItemType: "folder", Token: "group0000000001", Permissions: 1, ShareWith: "team", StimeMs: 1746100800000,
	}
	link := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeLink, Path: "/a.txt",
		ItemType: "file", Token: "link00000000001", Permissions: 1, StimeMs: 1746100800000,
	}
	for _, s := range []*files.Share{userShare, groupShare, link} {
		if err := store.Insert(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ListBySharee(ctx, "bob", nil)
	if err != nil || len(got) != 1 || got[0].ShareWith != "bob" {
		t.Fatalf("user sharee = %+v %v", got, err)
	}
	got, err = store.ListBySharee(ctx, "carol", []string{"team"})
	if err != nil || len(got) != 1 || got[0].ShareWith != "team" {
		t.Fatalf("group sharee = %+v %v", got, err)
	}
}

func TestSQLShareStoreDeleteExpired(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	now := int64(1746100800000)
	live := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeLink, Path: "/live.txt",
		ItemType: "file", Token: "live00000000001", Permissions: 1, ExpireMs: now + 1000, StimeMs: now,
	}
	dead := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeLink, Path: "/dead.txt",
		ItemType: "file", Token: "dead00000000001", Permissions: 1, ExpireMs: now - 1, StimeMs: now,
	}
	never := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeLink, Path: "/never.txt",
		ItemType: "file", Token: "never0000000001", Permissions: 1, StimeMs: now,
	}
	for _, s := range []*files.Share{live, dead, never} {
		if err := store.Insert(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := store.DeleteExpired(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != dead.ID {
		t.Fatalf("deleted ids = %v, want [%d]", ids, dead.ID)
	}
	if _, err := store.GetByToken(ctx, "dead00000000001"); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("expired still present: %v", err)
	}
	if _, err := store.GetByToken(ctx, "live00000000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByToken(ctx, "never0000000001"); err != nil {
		t.Fatal(err)
	}
	// Nothing left to expire: empty, not nil-error noise.
	ids, err = store.DeleteExpired(ctx, now)
	if err != nil || len(ids) != 0 {
		t.Fatalf("second sweep = %v, %v; want empty", ids, err)
	}
}

func TestSQLShareStoreCovering(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	mk := func(token string, shareType int, path, shareWith string) {
		t.Helper()
		sh := &files.Share{
			OwnerUserID: uid, ShareType: shareType, Path: path,
			ItemType: "folder", Token: token, Permissions: 1,
			StimeMs: 1746100800000, ShareWith: shareWith,
		}
		if err := store.Insert(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	mk("cover0000000001", files.ShareTypeUser, "/docs", "bob")          // ancestor of the file
	mk("cover0000000002", files.ShareTypeGroup, "/docs/sub", "g1")      // ancestor
	mk("cover0000000003", files.ShareTypeUser, "/docs/sub/f.txt", "bo") // exact target
	mk("cover0000000004", files.ShareTypeLink, "/docs", "")             // links never wrap
	mk("cover0000000005", files.ShareTypeRemote, "/docs", "x@h")        // OCM never wraps
	mk("cover0000000006", files.ShareTypeUser, "/elsewhere", "bob")     // not an ancestor
	mk("cover0000000007", files.ShareTypeUser, "/docs/sub/f.txt2", "b") // prefix but not ancestor

	got, err := store.Covering(ctx, uid, "/docs/sub/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	tokens := make([]string, 0, len(got))
	for _, sh := range got {
		tokens = append(tokens, sh.Token)
	}
	want := []string{"cover0000000001", "cover0000000002", "cover0000000003"}
	if len(tokens) != len(want) {
		t.Fatalf("covering = %v, want %v", tokens, want)
	}
	for i := range want {
		if tokens[i] != want[i] {
			t.Fatalf("covering = %v, want %v", tokens, want)
		}
	}

	// A root share covers everything below it.
	mk("cover0000000008", files.ShareTypeUser, "/", "bob")
	got, err = store.Covering(ctx, uid, "/new/deep/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Token != "cover0000000008" {
		t.Fatalf("covering under root share = %v", got)
	}

	// Another owner's shares never match.
	got, err = store.Covering(ctx, uid+999, "/docs/sub/f.txt")
	if err != nil || len(got) != 0 {
		t.Fatalf("covering for other owner = %v %v", got, err)
	}
}

func TestSQLShareStoreForGroup(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	mk := func(token string, shareType int, shareWith string) {
		t.Helper()
		sh := &files.Share{
			OwnerUserID: uid, ShareType: shareType, Path: "/x.txt",
			ItemType: "file", Token: token, Permissions: 1,
			StimeMs: 1746100800000, ShareWith: shareWith,
		}
		if err := store.Insert(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	mk("group0000000001", files.ShareTypeGroup, "g1")
	mk("group0000000002", files.ShareTypeGroup, "g1")
	mk("group0000000003", files.ShareTypeGroup, "g2")
	mk("group0000000004", files.ShareTypeUser, "g1") // type filter: user sharee named g1

	got, err := store.ForGroup(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Token != "group0000000001" || got[1].Token != "group0000000002" {
		t.Fatalf("for group = %v", got)
	}
	if got, err := store.ForGroup(ctx, ""); err != nil || got != nil {
		t.Fatalf("empty gid = %v %v", got, err)
	}
}

func TestSQLShareStoreListExpired(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	mk := func(token string, expireMs int64) {
		t.Helper()
		sh := &files.Share{
			OwnerUserID: uid, ShareType: files.ShareTypeUser, Path: "/x.txt",
			ItemType: "file", Token: token, Permissions: 1,
			ExpireMs: expireMs, StimeMs: 1746100800000, ShareWith: "bob",
		}
		if err := store.Insert(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	mk("expired00000001", now-1)
	mk("expired00000002", now)     // boundary: <= now is expired
	mk("expired00000003", now+100) // live
	mk("expired00000004", 0)       // no expiry

	got, err := store.ListExpired(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Token != "expired00000001" || got[1].Token != "expired00000002" {
		t.Fatalf("list expired = %v", got)
	}
	ids, err := store.DeleteExpired(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("delete expired ids = %v", ids)
	}
	if got[0].ID != ids[0] || got[1].ID != ids[1] {
		t.Fatalf("list/delete mismatch: %v vs %v", got, ids)
	}
}
