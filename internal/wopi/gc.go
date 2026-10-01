package wopi

import (
	"context"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

type gcJob struct {
	store Store
	clock func() time.Time
}

// NewGCJob deletes WOPI tokens whose expiry is in the past. Registered only
// when office.enabled is set; periodic via jobs.JobWOPITokensGC.
func NewGCJob(store Store, clock func() time.Time) jobs.Job {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &gcJob{store: store, clock: clock}
}

func (j *gcJob) Name() string { return jobs.JobWOPITokensGC }

func (j *gcJob) Run(ctx context.Context, _ []byte) error {
	if j == nil || j.store == nil {
		return nil
	}
	_, err := j.store.DeleteExpired(ctx, j.clock().UTC())
	return err
}
