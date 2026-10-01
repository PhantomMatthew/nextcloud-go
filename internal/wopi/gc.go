package wopi

import (
	"context"
	"log/slog"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

// TokenKeyCleaner is the GC-facing half of the token-bound key wraps
// (ADR-0107): bulk revocation of exactly the reaped tokens' wraps, plus the
// orphan purge as the safety net. *encrypt.SQLResolver satisfies it
// structurally; a nil keys disables the key-side sweep.
type TokenKeyCleaner interface {
	DeleteWOPITokenKeys(ctx context.Context, tokens []string) (int64, error)
	PruneWOPITokenKeys(ctx context.Context) (int64, error)
}

type gcJob struct {
	store  Store
	clock  func() time.Time
	keys   TokenKeyCleaner
	logger *slog.Logger
}

// NewGCJob deletes WOPI tokens whose expiry is in the past. When keys is
// non-nil the reaped tokens' key wraps are deleted first (ADR-0107
// cryptographic revocation: the wrap is keyed by the token, so it must go
// before the row it opens through) and orphans are pruned after — key-side
// errors are Warn-logged and never fail the row deletion (mirroring
// sharing's expire job). Registered only when office.enabled is set;
// periodic via jobs.JobWOPITokensGC.
func NewGCJob(store Store, clock func() time.Time, keys TokenKeyCleaner, logger *slog.Logger) jobs.Job {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &gcJob{store: store, clock: clock, keys: keys, logger: logger}
}

func (j *gcJob) Name() string { return jobs.JobWOPITokensGC }

func (j *gcJob) Run(ctx context.Context, _ []byte) error {
	if j == nil || j.store == nil {
		return nil
	}
	now := j.clock().UTC()
	if j.keys != nil {
		expired, err := j.store.ExpiredTokens(ctx, now)
		if err != nil && j.logger != nil {
			j.logger.WarnContext(ctx, "wopi: gc: list expired tokens failed, key wrap deletion skipped",
				slog.Any("err", err))
		}
		if len(expired) > 0 {
			if _, err := j.keys.DeleteWOPITokenKeys(ctx, expired); err != nil && j.logger != nil {
				j.logger.WarnContext(ctx, "wopi: gc: delete token key wraps failed", slog.Any("err", err))
			}
		}
	}
	if _, err := j.store.DeleteExpired(ctx, now); err != nil {
		return err
	}
	if j.keys != nil {
		if _, err := j.keys.PruneWOPITokenKeys(ctx); err != nil && j.logger != nil {
			j.logger.WarnContext(ctx, "wopi: gc: prune token key wraps failed", slog.Any("err", err))
		}
	}
	return nil
}
