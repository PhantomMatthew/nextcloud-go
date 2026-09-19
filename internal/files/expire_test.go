package files

import (
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

func TestExpireLocksJob(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLLockStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Insert(ctx, &FileLock{
		UserID: uid, Path: "/old.txt", Token: "opaquelocktoken:oldeoldeoldeoldeoldeoldeoldeolde",
		Owner: "alice", TimeoutMs: now.UnixMilli() - 1, CreatedMs: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	job := NewExpireLocksJob(store, func() time.Time { return now })
	if job.Name() != jobs.JobLocksExpire {
		t.Fatalf("name = %s", job.Name())
	}
	if err := job.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByPath(ctx, uid, "/old.txt"); err == nil {
		t.Fatal("expired lock still present")
	}
}
