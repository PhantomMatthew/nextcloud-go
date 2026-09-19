package sharing

import (
	"context"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

type expireSharesJob struct {
	store files.ShareStore
	clock func() time.Time
}

// NewExpireJob deletes rows whose expire_ms is in the past.
func NewExpireJob(store files.ShareStore, clock func() time.Time) jobs.Job {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &expireSharesJob{store: store, clock: clock}
}

func (j *expireSharesJob) Name() string { return jobs.JobSharesExpire }

func (j *expireSharesJob) Run(ctx context.Context, _ []byte) error {
	if j == nil || j.store == nil {
		return nil
	}
	return j.store.DeleteExpired(ctx, j.clock().UTC().UnixMilli())
}
