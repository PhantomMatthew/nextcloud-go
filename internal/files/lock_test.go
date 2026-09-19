package files

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func newLockDAV(t *testing.T) *DAV {
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
	dav.Locks = NewSQLLockStore(db)
	dav.Props = NewSQLPropertyStore(db)
	tr := NewTrash(st, NewSQLTrashStore(db), dav, us)
	dav.Trash = tr
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return freeze }
	tr.Clock = func() time.Time { return freeze }
	dav.NewToken = func() string { return "opaquelocktoken:ncgo0000000000000000000000000001" }
	return dav
}

func TestDAVLockUnlockCheck(t *testing.T) {
	ctx := t.Context()
	dav := newLockDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	info, err := dav.Lock(ctx, "alice", "/a.txt", webdav.LockRequest{Owner: "alice", Timeout: 1800 * time.Second})
	if err != nil || info == nil || info.Token == "" {
		t.Fatalf("lock = %+v err=%v", info, err)
	}
	if err := dav.CheckLock(ctx, "alice", "/a.txt", ""); !errors.Is(err, webdav.ErrLocked) {
		t.Fatalf("check without token = %v", err)
	}
	if err := dav.CheckLock(ctx, "alice", "/a.txt", "(<"+info.Token+">)"); err != nil {
		t.Fatalf("check with token = %v", err)
	}
	if _, err := dav.Lock(ctx, "alice", "/a.txt", webdav.LockRequest{Owner: "alice"}); !errors.Is(err, webdav.ErrLocked) {
		t.Fatalf("second lock = %v", err)
	}
	refreshed, err := dav.Lock(ctx, "alice", "/a.txt", webdav.LockRequest{
		Refresh: true,
		Token:   info.Token,
		Timeout: 3600 * time.Second,
	})
	if err != nil || refreshed.Timeout < 3500*time.Second {
		t.Fatalf("refresh = %+v err=%v", refreshed, err)
	}
	if err := dav.Unlock(ctx, "alice", "/a.txt", "opaquelocktoken:deadbeefdeadbeefdeadbeefdeadbeef"); !errors.Is(err, webdav.ErrConflict) {
		t.Fatalf("wrong unlock = %v", err)
	}
	if err := dav.Unlock(ctx, "alice", "/a.txt", info.Token); err != nil {
		t.Fatal(err)
	}
	if err := dav.CheckLock(ctx, "alice", "/a.txt", ""); err != nil {
		t.Fatalf("after unlock = %v", err)
	}
}

func TestDAVLockExpire(t *testing.T) {
	ctx := t.Context()
	dav := newLockDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return now }
	if _, err := dav.Lock(ctx, "alice", "/a.txt", webdav.LockRequest{Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	dav.Clock = func() time.Time { return now.Add(2 * time.Second) }
	if err := dav.CheckLock(ctx, "alice", "/a.txt", ""); err != nil {
		t.Fatalf("expired lock still held: %v", err)
	}
}

func TestDAVLockMovePurgeTrash(t *testing.T) {
	ctx := t.Context()
	dav := newLockDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	info, err := dav.Lock(ctx, "alice", "/a.txt", webdav.LockRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Copy(ctx, "alice", "/a.txt", "alice", "/copied.txt", false, true); err != nil {
		t.Fatal(err)
	}
	if err := dav.CheckLock(ctx, "alice", "/copied.txt", ""); err != nil {
		t.Fatalf("copy must not copy lock: %v", err)
	}
	if _, _, err := dav.Move(ctx, "alice", "/a.txt", "alice", "/b.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := dav.CheckLock(ctx, "alice", "/a.txt", ""); err != nil {
		t.Fatalf("moved source lock: %v", err)
	}
	if err := dav.CheckLock(ctx, "alice", "/b.txt", ""); !errors.Is(err, webdav.ErrLocked) {
		t.Fatalf("moved dest lock = %v", err)
	}
	if err := dav.CheckLock(ctx, "alice", "/b.txt", "(<"+info.Token+">)"); err != nil {
		t.Fatalf("moved dest with token = %v", err)
	}
	if err := dav.Purge(ctx, "alice", "/copied.txt"); err != nil {
		t.Fatal(err)
	}
	u, err := dav.Users.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := dav.Remove(ctx, "alice", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Locks.GetByPath(ctx, u.ID, "/b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trash left lock: %v", err)
	}
}

func TestDAVLockMissing(t *testing.T) {
	ctx := t.Context()
	dav := newLockDAV(t)
	if _, err := dav.Lock(ctx, "alice", "/missing.txt", webdav.LockRequest{}); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("missing lock = %v", err)
	}
}

func TestDAVLockDepthInfinity(t *testing.T) {
	ctx := t.Context()
	dav := newLockDAV(t)
	if _, err := dav.Mkdir(ctx, "alice", "/dir"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/dir/a.txt", strings.NewReader("hi"), nil); err != nil {
		t.Fatal(err)
	}
	info, err := dav.Lock(ctx, "alice", "/dir", webdav.LockRequest{Owner: "alice", DepthInfinity: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := dav.CheckLock(ctx, "alice", "/dir/a.txt", ""); !errors.Is(err, webdav.ErrLocked) {
		t.Fatalf("child without token = %v", err)
	}
	if err := dav.CheckLock(ctx, "alice", "/dir/a.txt", "(<"+info.Token+">)"); err != nil {
		t.Fatalf("child with token = %v", err)
	}
	if err := dav.Unlock(ctx, "alice", "/dir", info.Token); err != nil {
		t.Fatal(err)
	}
	if err := dav.CheckLock(ctx, "alice", "/dir/a.txt", ""); err != nil {
		t.Fatalf("after unlock = %v", err)
	}
}
