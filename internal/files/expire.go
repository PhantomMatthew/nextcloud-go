package files

import (
	"context"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

type expireLocksJob struct {
	store LockStore
	clock func() time.Time
}

// NewExpireLocksJob deletes locks whose timeout_ms is in the past.
func NewExpireLocksJob(store LockStore, clock func() time.Time) jobs.Job {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &expireLocksJob{store: store, clock: clock}
}

func (j *expireLocksJob) Name() string { return jobs.JobLocksExpire }

func (j *expireLocksJob) Run(ctx context.Context, _ []byte) error {
	if j == nil || j.store == nil {
		return nil
	}
	return j.store.DeleteExpired(ctx, j.clock().UTC().UnixMilli())
}
