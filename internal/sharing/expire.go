package sharing

import (
	"context"
	"log/slog"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

type expireSharesJob struct {
	store  files.ShareStore
	clock  func() time.Time
	notifs ShareNotifier
	logger *slog.Logger
}

// NewExpireJob deletes rows whose expire_ms is in the past. When notifs is
// non-nil the bells of every reaped share are dismissed too (ADR-0083): this
// background path bulk-deletes without going through Service.Delete, so
// without the dismissal a share reaped here before any read would leave its
// notifications behind.
func NewExpireJob(store files.ShareStore, clock func() time.Time, notifs ShareNotifier, logger *slog.Logger) jobs.Job {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &expireSharesJob{store: store, clock: clock, notifs: notifs, logger: logger}
}

func (j *expireSharesJob) Name() string { return jobs.JobSharesExpire }

func (j *expireSharesJob) Run(ctx context.Context, _ []byte) error {
	if j == nil || j.store == nil {
		return nil
	}
	ids, err := j.store.DeleteExpired(ctx, j.clock().UTC().UnixMilli())
	if err != nil {
		return err
	}
	if j.notifs == nil {
		return nil
	}
	for _, id := range ids {
		// Best-effort, same as the service paths: a bell failure never
		// changes the sweep's outcome.
		if derr := j.notifs.DeleteByObject(ctx, "share", shareNotifObjectID(id)); derr != nil && j.logger != nil {
			j.logger.WarnContext(ctx, "sharing: expire: dismiss notification failed", slog.Int64("share", id), slog.Any("err", derr))
		}
	}
	return nil
}
