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

// newWOPILockTestDAV builds a DAV with a lock store and an ADVANCEABLE
// frozen clock (the pointer lets a test step past a lock's expiry).
func newWOPILockTestDAV(t *testing.T) (*DAV, *users.User, *time.Time) {
	t.Helper()
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := us.Create(ctx, &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	dav.Locks = NewSQLLockStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return now }
	return dav, u, &now
}

func TestLockWithTokenVerbatim(t *testing.T) {
	ctx := t.Context()
	dav, _, _ := newWOPILockTestDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	info, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "wopi-abc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.Token != "wopi-abc" {
		t.Fatalf("token = %q, want verbatim wopi-abc", info.Token)
	}
	tok, locked, err := dav.LockTokenAt(ctx, "alice", "/doc.odt")
	if err != nil || !locked || tok != "wopi-abc" {
		t.Fatalf("LockTokenAt = %q %v %v", tok, locked, err)
	}
	// No opaquelocktoken: minting anywhere: the row stores the client id.
	row, err := dav.Locks.GetByPath(ctx, 1, "/doc.odt")
	if err != nil {
		t.Fatal(err)
	}
	if row.Token != "wopi-abc" || strings.HasPrefix(row.Token, "opaquelocktoken:") {
		t.Fatalf("stored token = %q", row.Token)
	}
}

func TestLockWithTokenConflictAndIdempotent(t *testing.T) {
	ctx := t.Context()
	dav, _, _ := newWOPILockTestDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "wopi-abc", 0); err != nil {
		t.Fatal(err)
	}
	// Same token re-LOCK is the idempotent refresh (200 in WOPI terms).
	if _, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "wopi-abc", 0); err != nil {
		t.Fatalf("re-lock same token = %v", err)
	}
	if _, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "wopi-other", 0); !errors.Is(err, webdav.ErrLocked) {
		t.Fatalf("lock with different token = %v, want ErrLocked", err)
	}
	if _, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "", 0); !errors.Is(err, webdav.ErrBadRequest) {
		t.Fatalf("empty token = %v, want ErrBadRequest", err)
	}
}

func TestUnlockWithToken(t *testing.T) {
	ctx := t.Context()
	dav, _, _ := newWOPILockTestDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "wopi-abc", 0); err != nil {
		t.Fatal(err)
	}
	if err := dav.UnlockWithToken(ctx, "alice", "/doc.odt", "wopi-wrong"); !errors.Is(err, webdav.ErrConflict) {
		t.Fatalf("unlock mismatch = %v, want ErrConflict", err)
	}
	if err := dav.UnlockWithToken(ctx, "alice", "/doc.odt", "wopi-abc"); err != nil {
		t.Fatalf("unlock match = %v", err)
	}
	if _, locked, err := dav.LockTokenAt(ctx, "alice", "/doc.odt"); err != nil || locked {
		t.Fatalf("after unlock locked=%v err=%v", locked, err)
	}
	// No lock present: UNLOCK is a conflict (WOPI 409).
	if err := dav.UnlockWithToken(ctx, "alice", "/doc.odt", "wopi-abc"); !errors.Is(err, webdav.ErrConflict) {
		t.Fatalf("unlock without lock = %v, want ErrConflict", err)
	}
}

func TestLockTokenAtExpiry(t *testing.T) {
	ctx := t.Context()
	dav, _, now := newWOPILockTestDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "wopi-abc", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, locked, _ := dav.LockTokenAt(ctx, "alice", "/doc.odt"); !locked {
		t.Fatal("fresh lock not reported")
	}
	*now = now.Add(2 * time.Second)
	if _, locked, err := dav.LockTokenAt(ctx, "alice", "/doc.odt"); err != nil || locked {
		t.Fatalf("expired lock: locked=%v err=%v", locked, err)
	}
	// The expired row is gone, so a different client may lock immediately.
	if _, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "wopi-next", 0); err != nil {
		t.Fatalf("lock after expiry = %v", err)
	}
}

func TestLockWithTokenIncomingShare(t *testing.T) {
	ctx := t.Context()
	dav, alice, _ := newWOPILockTestDAV(t)
	if _, err := dav.Mkdir(ctx, "alice", "/shared"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/shared/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	dav.Incoming = stubIncoming{mounts: []IncomingMount{{
		OwnerUID: "alice", OwnerPath: "/shared", Mount: "/shared",
		Permissions: webdav.PermRead | webdav.PermUpdate, ItemType: "folder",
	}}}
	// The sharee locks through the MOUNT path; the row lands in the owner's
	// lock namespace so owner and sharee contend on one lock.
	if _, err := dav.LockWithToken(ctx, "bob", "/shared/doc.odt", "wopi-bob", 0); err != nil {
		t.Fatal(err)
	}
	row, err := dav.Locks.GetByPath(ctx, alice.ID, "/shared/doc.odt")
	if err != nil {
		t.Fatal(err)
	}
	if row.Token != "wopi-bob" {
		t.Fatalf("owner-namespace token = %q", row.Token)
	}
	// The owner sees the same lock through her own path.
	tok, locked, err := dav.LockTokenAt(ctx, "alice", "/shared/doc.odt")
	if err != nil || !locked || tok != "wopi-bob" {
		t.Fatalf("owner LockTokenAt = %q %v %v", tok, locked, err)
	}
	// Mismatch from either side conflicts; the matching id releases.
	if err := dav.UnlockWithToken(ctx, "alice", "/shared/doc.odt", "wopi-alice"); !errors.Is(err, webdav.ErrConflict) {
		t.Fatalf("owner unlock mismatch = %v, want ErrConflict", err)
	}
	if err := dav.UnlockWithToken(ctx, "bob", "/shared/doc.odt", "wopi-bob"); err != nil {
		t.Fatalf("sharee unlock = %v", err)
	}
	if _, locked, _ := dav.LockTokenAt(ctx, "alice", "/shared/doc.odt"); locked {
		t.Fatal("lock still present after sharee unlock")
	}
}

func TestLockWithTokenNilStore(t *testing.T) {
	ctx := t.Context()
	dav, _, _ := newWOPILockTestDAV(t)
	dav.Locks = nil
	if _, _, err := dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.LockWithToken(ctx, "alice", "/doc.odt", "wopi-abc", 0); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("nil store lock = %v, want ErrForbidden", err)
	}
	if err := dav.UnlockWithToken(ctx, "alice", "/doc.odt", "wopi-abc"); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("nil store unlock = %v, want ErrForbidden", err)
	}
	if _, locked, err := dav.LockTokenAt(ctx, "alice", "/doc.odt"); err != nil || locked {
		t.Fatalf("nil store LockTokenAt = %v %v, want unlocked nil", locked, err)
	}
}
