package files

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
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

func TestSQLStoreFilecache(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)
	uid := seedUser(t, db)

	root, err := store.EnsureRoot(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if !root.IsDir || root.Path != "/" || root.ID == 0 {
		t.Fatalf("root = %+v", root)
	}
	again, err := store.EnsureRoot(ctx, uid)
	if err != nil || again.ID != root.ID {
		t.Fatalf("ensure again = %+v %v", again, err)
	}

	dir := &File{UserID: uid, Path: "/docs", IsDir: true, Permissions: 31}
	if err := store.Insert(ctx, dir); err != nil {
		t.Fatal(err)
	}
	file := &File{UserID: uid, Path: "/docs/a.txt", Size: 5, MIME: "text/plain", Permissions: 31}
	if err := store.Insert(ctx, file); err != nil {
		t.Fatal(err)
	}
	if file.ID == 0 || file.ETag == "" || file.ETag == "pending" {
		t.Fatalf("file etag not assigned: %+v", file)
	}

	now := time.Unix(1_800_000_000, 0).UTC()
	if err := store.RecalcAncestors(ctx, uid, file.ParentID, now); err != nil {
		t.Fatal(err)
	}
	docs, err := store.GetByPath(ctx, uid, "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if docs.Size != 5 {
		t.Errorf("docs size = %d", docs.Size)
	}
	root, err = store.GetByPath(ctx, uid, "/")
	if err != nil {
		t.Fatal(err)
	}
	if root.Size != 5 {
		t.Errorf("root size = %d", root.Size)
	}
	oldDocsETag := docs.ETag

	file.Size = 9
	file.ETag = ComputeFileETag(file.ID, now, 9)
	file.Mtime = now
	if err := store.UpdateMeta(ctx, file); err != nil {
		t.Fatal(err)
	}
	if err := store.RecalcAncestors(ctx, uid, file.ParentID, now); err != nil {
		t.Fatal(err)
	}
	docs, err = store.GetByPath(ctx, uid, "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if docs.Size != 9 {
		t.Errorf("docs size after update = %d", docs.Size)
	}
	if docs.ETag == oldDocsETag {
		t.Error("docs etag did not change")
	}

	n, err := store.Usage(ctx, uid)
	if err != nil || n != 9 {
		t.Errorf("usage = %d %v", n, err)
	}

	if err := store.RenameSubtree(ctx, uid, "/docs", "/papers"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByPath(ctx, uid, "/docs"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old path: %v", err)
	}
	moved, err := store.GetByPath(ctx, uid, "/papers/a.txt")
	if err != nil || moved.Size != 9 {
		t.Fatalf("moved = %+v %v", moved, err)
	}

	if err := store.DeleteSubtree(ctx, uid, "/papers"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByPath(ctx, uid, "/papers/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("child after delete: %v", err)
	}
	if _, err := store.GetByPath(ctx, uid, "/"); err != nil {
		t.Errorf("root missing: %v", err)
	}

	if err := store.DeleteSubtree(ctx, uid, "/"); !errors.Is(err, ErrForbidden) {
		t.Errorf("delete root = %v", err)
	}
	if _, err := store.GetByPath(ctx, uid, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}

func TestInsertRequiresParent(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)
	uid := seedUser(t, db)
	if _, err := store.EnsureRoot(ctx, uid); err != nil {
		t.Fatal(err)
	}
	f := &File{UserID: uid, Path: "/missing/a.txt", Size: 1}
	if err := store.Insert(ctx, f); !errors.Is(err, ErrParentMissing) {
		t.Errorf("insert = %v", err)
	}
}
