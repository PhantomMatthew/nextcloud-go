package files

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func newTrash(t *testing.T) (*Trash, *DAV) {
	t.Helper()
	ctx := t.Context()
	db := testDB(t)
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
	tr := NewTrash(st, NewSQLTrashStore(db), dav, us)
	dav.Trash = tr
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return freeze }
	tr.Clock = func() time.Time { return freeze }
	return tr, dav
}

func TestTrashMoveRestorePurge(t *testing.T) {
	ctx := t.Context()
	tr, dav := newTrash(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if err := dav.Remove(ctx, "alice", "/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Stat(ctx, "alice", "/a.txt"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("still in files: %v", err)
	}
	ents, err := tr.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash list = %v %v", ents, err)
	}
	if ents[0].TrashOriginal != "a.txt" || ents[0].TrashDeleted != 1746100800 {
		t.Fatalf("props = %+v", ents[0])
	}
	loc := strings.TrimPrefix(ents[0].Path, "/")
	rc, _, err := tr.Read(ctx, "alice", "/trash/"+loc)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || string(body) != "hello" {
		t.Fatalf("trash body=%q err=%v", body, err)
	}

	got, created, err := tr.Restore(ctx, "alice", loc, "alice", "", true)
	if err != nil || !created || got.Size != 5 {
		t.Fatalf("restore = %+v created=%v err=%v", got, created, err)
	}
	rc, _, err = dav.Read(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || string(body) != "hello" {
		t.Fatalf("restored body=%q err=%v", body, err)
	}
	if _, err := tr.Stat(ctx, "alice", "/trash/"+loc); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("trash item remains: %v", err)
	}

	if err := dav.Remove(ctx, "alice", "/a.txt"); err != nil {
		t.Fatal(err)
	}
	ents, err = tr.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash after second delete = %v %v", ents, err)
	}
	loc = strings.TrimPrefix(ents[0].Path, "/")
	if err := tr.Remove(ctx, "alice", "/trash/"+loc); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Stat(ctx, "alice", "/trash/"+loc); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("purge left item: %v", err)
	}
	if _, err := dav.Stat(ctx, "alice", "/a.txt"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("purged file restored: %v", err)
	}
}

func TestTrashOverwriteUsesPurge(t *testing.T) {
	ctx := t.Context()
	tr, dav := newTrash(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("one"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/b.txt", strings.NewReader("two"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Move(ctx, "alice", "/b.txt", "alice", "/a.txt", true); err != nil {
		t.Fatal(err)
	}
	ents, err := tr.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 0 {
		t.Fatalf("overwrite should purge not trash: %v %v", ents, err)
	}
	rc, _, err := dav.Read(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || string(body) != "two" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

func TestTrashLazyExpire(t *testing.T) {
	ctx := t.Context()
	tr, dav := newTrash(t)
	tr.Retention = time.Second
	if _, _, err := dav.Write(ctx, "alice", "/old.txt", bytes.NewReader([]byte("x")), nil); err != nil {
		t.Fatal(err)
	}
	if err := dav.Remove(ctx, "alice", "/old.txt"); err != nil {
		t.Fatal(err)
	}
	tr.Clock = func() time.Time {
		return time.Date(2025, 5, 1, 12, 0, 2, 0, time.UTC)
	}
	ents, err := tr.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 0 {
		t.Fatalf("expired still listed: %v %v", ents, err)
	}
}
