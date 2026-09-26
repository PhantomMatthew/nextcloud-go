package sharing

import (
	"context"
	"log/slog"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

// expiredShareLister is the optional store capability the expire job uses to
// read the doomed rows before reaping them, so recipient key wraps can be
// dropped for exactly the deleted shares (ADR-0098). *SQLShareStore
// satisfies it.
type expiredShareLister interface {
	ListExpired(ctx context.Context, nowMs int64) ([]*files.Share, error)
}

type expireSharesJob struct {
	store  files.ShareStore
	clock  func() time.Time
	notifs ShareNotifier
	keys   ShareKeys
	logger *slog.Logger
}

// NewExpireJob deletes rows whose expire_ms is in the past. When notifs is
// non-nil the bells of every reaped share are dismissed too (ADR-0083): this
// background path bulk-deletes without going through Service.Delete, so
// without the dismissal a share reaped here before any read would leave its
// notifications behind. When keys is non-nil the recipients' key wraps are
// dropped the same way (ADR-0098): the job lists the doomed rows first and
// unwraps exactly the shares it deleted.
func NewExpireJob(store files.ShareStore, clock func() time.Time, notifs ShareNotifier, logger *slog.Logger, keys ShareKeys) jobs.Job {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &expireSharesJob{store: store, clock: clock, notifs: notifs, keys: keys, logger: logger}
}

func (j *expireSharesJob) Name() string { return jobs.JobSharesExpire }

func (j *expireSharesJob) Run(ctx context.Context, _ []byte) error {
	if j == nil || j.store == nil {
		return nil
	}
	nowMs := j.clock().UTC().UnixMilli()
	var doomed []*files.Share
	if j.keys != nil {
		if l, ok := j.store.(expiredShareLister); ok {
			listed, err := l.ListExpired(ctx, nowMs)
			if err != nil && j.logger != nil {
				j.logger.WarnContext(ctx, "sharing: expire: list doomed shares failed, key unwrap skipped",
					slog.Any("err", err))
			}
			doomed = listed
		}
	}
	ids, err := j.store.DeleteExpired(ctx, nowMs)
	if err != nil {
		return err
	}
	reaped := make(map[int64]bool, len(ids))
	for _, id := range ids {
		reaped[id] = true
	}
	if j.notifs != nil {
		for _, id := range ids {
			// Best-effort, same as the service paths: a bell failure never
			// changes the sweep's outcome.
			if derr := j.notifs.DeleteByObject(ctx, "share", shareNotifObjectID(id)); derr != nil && j.logger != nil {
				j.logger.WarnContext(ctx, "sharing: expire: dismiss notification failed", slog.Int64("share", id), slog.Any("err", derr))
			}
		}
	}
	if j.keys != nil {
		for _, sh := range doomed {
			if !reaped[sh.ID] {
				continue
			}
			// Best-effort: recipient rows are not a read-path dependency.
			if uerr := j.keys.UnwrapForShare(ctx, sh); uerr != nil && j.logger != nil {
				j.logger.WarnContext(ctx, "sharing: expire: share key unwrap failed", slog.Int64("share", sh.ID), slog.Any("err", uerr))
			}
		}
	}
	return nil
}
