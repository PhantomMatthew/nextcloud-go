package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func TestDAVRoundTrip(t *testing.T) {
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
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)

	if _, err := dav.Mkdir(ctx, "alice", "/docs"); err != nil {
		t.Fatal(err)
	}
	ent, created, err := dav.Write(ctx, "alice", "/docs/a.txt", bytes.NewReader([]byte("hello")), nil)
	if err != nil || !created || ent.Size != 5 {
		t.Fatalf("write = %+v created=%v err=%v", ent, created, err)
	}
	if ent.ETag == "" {
		t.Fatal("missing etag")
	}

	stRoot, err := dav.Stat(ctx, "alice", "/")
	if err != nil || !stRoot.IsDir {
		t.Fatalf("stat root = %+v %v", stRoot, err)
	}
	kids, err := dav.List(ctx, "alice", "/")
	if err != nil || len(kids) != 1 {
		t.Fatalf("list root = %v %v", kids, err)
	}
	docs, err := dav.Stat(ctx, "alice", "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if docs.Size != 5 {
		t.Errorf("docs size = %d", docs.Size)
	}

	rc, got, err := dav.Read(ctx, "alice", "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "hello" || got.Size != 5 {
		t.Fatalf("read = %q %+v %v", body, got, err)
	}

	if _, _, err := dav.Write(ctx, "alice", "/docs/a.txt", bytes.NewReader([]byte("hello!")), nil); err != nil {
		t.Fatal(err)
	}
	docs2, err := dav.Stat(ctx, "alice", "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if docs2.ETag == docs.ETag {
		t.Error("parent etag unchanged after rewrite")
	}

	if err := dav.Remove(ctx, "alice", "/docs/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Stat(ctx, "alice", "/docs/a.txt"); !errors.Is(err, webdav.ErrNotFound) {
		t.Errorf("removed = %v", err)
	}
}
