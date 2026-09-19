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
	job := NewExpireJob(store, func() time.Time { return now })
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
