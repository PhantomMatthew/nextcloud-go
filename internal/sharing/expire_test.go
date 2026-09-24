package sharing

import (
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

func TestExpireSharesJob(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	sh := &files.Share{
		OwnerUserID: uid,
		ShareType:   files.ShareTypeLink,
		Path:        "/old.txt",
		ItemType:    "file",
		Token:       "expire000000001",
		Permissions: 1,
		ExpireMs:    now.UnixMilli() - 1,
		StimeMs:     now.UnixMilli(),
	}
	if err := store.Insert(ctx, sh); err != nil {
		t.Fatal(err)
	}
	job := NewExpireJob(store, func() time.Time { return now }, nil, nil)
	if job.Name() != jobs.JobSharesExpire {
		t.Fatalf("name = %s", job.Name())
	}
	if err := job.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByToken(ctx, "expire000000001"); err == nil {
		t.Fatal("expired share still present")
	}
}

// TestExpireSharesJobDismissesNotifications pins the ADR-0083 seam: the
// background sweep bulk-deletes without Service.Delete, so it must dismiss
// the reaped shares' bells itself — and a failing notifier must not fail
// the sweep.
func TestExpireSharesJobDismissesNotifications(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLShareStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dead := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeUser, Path: "/old.txt",
		ItemType: "file", Token: "expire000000001", Permissions: 1,
		ExpireMs: now.UnixMilli() - 1, StimeMs: now.UnixMilli(), ShareWith: "bob",
	}
	live := &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeUser, Path: "/live.txt",
		ItemType: "file", Token: "expire000000002", Permissions: 1,
		ExpireMs: now.UnixMilli() + 1000, StimeMs: now.UnixMilli(), ShareWith: "bob",
	}
	for _, sh := range []*files.Share{dead, live} {
		if err := store.Insert(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recordNotifier{}
	job := NewExpireJob(store, func() time.Time { return now }, rec, nil)
	if err := job.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByID(ctx, dead.ID); err == nil {
		t.Fatal("expired share still present")
	}
	if _, err := store.GetByID(ctx, live.ID); err != nil {
		t.Fatal("live share reaped")
	}
	want := [2]string{"share", shareNotifObjectID(dead.ID)}
	if len(rec.deleted) != 1 || rec.deleted[0] != want {
		t.Fatalf("dismissed = %v, want exactly %v", rec.deleted, want)
	}

	// A failing notifier never fails the sweep.
	store2 := NewSQLShareStore(db)
	if err := store2.Insert(ctx, &files.Share{
		OwnerUserID: uid, ShareType: files.ShareTypeLink, Path: "/gone.txt",
		ItemType: "file", Token: "expire000000003", Permissions: 1,
		ExpireMs: now.UnixMilli() - 1, StimeMs: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	job = NewExpireJob(store2, func() time.Time { return now }, failNotifier{}, nil)
	if err := job.Run(ctx, nil); err != nil {
		t.Fatalf("Run with failing notifier = %v, want nil", err)
	}
	if _, err := store2.GetByToken(ctx, "expire000000003"); err == nil {
		t.Fatal("expired share still present after failing-notifier run")
	}
}
