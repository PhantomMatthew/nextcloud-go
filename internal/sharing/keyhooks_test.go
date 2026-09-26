package sharing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
)

// recordShareKeys is a ShareKeys stub recording every call.
type recordShareKeys struct {
	wrapped   []*files.Share
	unwrapped []*files.Share
	err       error
}

func (r *recordShareKeys) WrapForShare(_ context.Context, sh *files.Share) error {
	r.wrapped = append(r.wrapped, sh)
	return r.err
}

func (r *recordShareKeys) UnwrapForShare(_ context.Context, sh *files.Share) error {
	r.unwrapped = append(r.unwrapped, sh)
	return r.err
}

func TestServiceShareKeyHooks(t *testing.T) {
	svc := testService(t)
	rec := &recordShareKeys{}
	svc.Keys = rec
	ctx := t.Context()
	tok := 0
	svc.NewToken = func() string {
		tok++
		return fmt.Sprintf("keyhook%09d0000", tok)
	}

	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeUser, 0, "bob", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.wrapped) != 1 || rec.wrapped[0].ID != sh.ID {
		t.Fatalf("wrapped = %v, want the created share", rec.wrapped)
	}

	// Link shares do not fire the grant hook (no wrappable recipient).
	if _, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeLink, 0, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.wrapped) != 1 {
		t.Fatalf("wrapped after link create = %v, want unchanged", rec.wrapped)
	}

	if err := svc.Delete(ctx, "alice", sh.ID); err != nil {
		t.Fatal(err)
	}
	if len(rec.unwrapped) != 1 || rec.unwrapped[0].ID != sh.ID {
		t.Fatalf("unwrapped = %v, want the deleted share", rec.unwrapped)
	}
}

// TestServiceShareKeyHooksBestEffort pins the ADR-0098 contract: a failing
// key hook never fails the share operation (recipient rows are not a
// read-path dependency until phase 4).
func TestServiceShareKeyHooksBestEffort(t *testing.T) {
	svc := testService(t)
	svc.Keys = &recordShareKeys{err: errors.New("keys down")}
	ctx := t.Context()

	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeUser, 0, "bob", "", "", "", "")
	if err != nil {
		t.Fatalf("Create with failing key hook = %v, want nil", err)
	}
	if err := svc.Delete(ctx, "alice", sh.ID); err != nil {
		t.Fatalf("Delete with failing key hook = %v, want nil", err)
	}
}

// TestServiceLazyExpireUnwrapsKeys covers the inline expiry path
// (GetForOwner's expireIfNeeded): a share reaped there unwraps too.
func TestServiceLazyExpireUnwrapsKeys(t *testing.T) {
	svc := testService(t) // clock frozen at 2025-05-01
	rec := &recordShareKeys{}
	svc.Keys = rec
	ctx := t.Context()

	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeUser, 0, "bob", "", "2025-01-01", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetForOwner(ctx, "alice", sh.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("GetForOwner of an expired share = %v, want ErrNotFound", err)
	}
	if len(rec.unwrapped) != 1 || rec.unwrapped[0].ID != sh.ID {
		t.Fatalf("unwrapped after lazy expiry = %v, want the expired share", rec.unwrapped)
	}
}

func TestExpireSharesJobUnwrapsKeys(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dead := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeUser, Path: "/old.txt",
		ItemType: "file", Token: "expirekeys000001", Permissions: 1,
		ExpireMs: now.UnixMilli() - 1, StimeMs: now.UnixMilli(), ShareWith: "bob",
	}
	live := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeUser, Path: "/live.txt",
		ItemType: "file", Token: "expirekeys000002", Permissions: 1,
		ExpireMs: now.UnixMilli() + 1000, StimeMs: now.UnixMilli(), ShareWith: "bob",
	}
	for _, sh := range []*files.Share{dead, live} {
		if err := store.Insert(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recordShareKeys{}
	job := NewExpireJob(store, func() time.Time { return now }, nil, nil, rec)
	if err := job.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(rec.unwrapped) != 1 || rec.unwrapped[0].ID != dead.ID {
		t.Fatalf("unwrapped = %v, want exactly the reaped share", rec.unwrapped)
	}

	// A failing hook never fails the sweep.
	dead2 := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeUser, Path: "/gone.txt",
		ItemType: "file", Token: "expirekeys000003", Permissions: 1,
		ExpireMs: now.UnixMilli() - 1, StimeMs: now.UnixMilli(), ShareWith: "bob",
	}
	if err := store.Insert(ctx, dead2); err != nil {
		t.Fatal(err)
	}
	job = NewExpireJob(store, func() time.Time { return now }, nil, nil, &recordShareKeys{err: errors.New("keys down")})
	if err := job.Run(ctx, nil); err != nil {
		t.Fatalf("Run with failing key hook = %v, want nil", err)
	}
	if _, err := store.GetByToken(ctx, "expirekeys000003"); err == nil {
		t.Fatal("expired share still present after failing-hook run")
	}
}
