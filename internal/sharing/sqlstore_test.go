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
