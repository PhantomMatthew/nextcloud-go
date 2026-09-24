package sharing

import (
	"context"
	"errors"
	"log/slog"
	"testing"

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
