package preview

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

const (
	// defaultGCMaxAge applies when the constructor receives maxAge <= 0.
	defaultGCMaxAge = 720 * time.Hour
	// gcMinInterval throttles the sweep: the periodic runner re-enqueues
	// after every poll (seconds by default), but a full cache listing is
	// daily work. In-memory only — a process restart costs one immediate
	// sweep, which is harmless.
	gcMinInterval = 24 * time.Hour
)

// gcJob deletes preview cache entries whose modtime is older than maxAge
// (ADR-0085). Cache keys are one-way content hashes, so orphaned entries
// cannot be detected exactly; sweeping by TTL is safe because deleting a
// still-reachable entry costs one regeneration and can never serve wrong
// bytes. With raw/encPrefix set (ADR-0105 §2) the NCGOPV1 self-sealed cache
// is swept with identical semantics on the undecorated backend — the
// decorator never sees those blobs. (TTL is modtime-based; no size
// accounting consumes these listings, and SealOverhead pins the
// plaintext/stored size delta for any future one.)
type gcJob struct {
	cache     storage.Storage
	prefix    string
	raw       storage.Storage
	encPrefix string
	maxAge    time.Duration
	clock     func() time.Time
	logger    *slog.Logger

	mu      sync.Mutex
	lastRun time.Time
}

// NewGCJob returns the jobs.Job sweeping prefix for entries older than
// maxAge; maxAge <= 0 selects the 720h default and a nil clock defaults to
// time.Now UTC. Runs are throttled to at most one sweep per 24h so the
// periodic re-enqueue cadence does not re-list the whole cache every poll
// interval.
func NewGCJob(cache storage.Storage, prefix string, maxAge time.Duration, clock func() time.Time, logger *slog.Logger) jobs.Job {
	return NewGCJobSealed(cache, prefix, nil, "", maxAge, clock, logger)
}

// NewGCJobSealed is NewGCJob plus a second sweep over encPrefix on the raw
// (undecorated) backend — the NCGOPV1 self-sealed preview cache (ADR-0105
// §2). A nil raw or empty encPrefix sweeps the legacy prefix only,
// bit-identical to NewGCJob.
func NewGCJobSealed(cache storage.Storage, prefix string, raw storage.Storage, encPrefix string, maxAge time.Duration, clock func() time.Time, logger *slog.Logger) jobs.Job {
	if maxAge <= 0 {
		maxAge = defaultGCMaxAge
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &gcJob{cache: cache, prefix: prefix, raw: raw, encPrefix: encPrefix, maxAge: maxAge, clock: clock, logger: logger}
}

func (j *gcJob) Name() string { return jobs.JobPreviewGC }

// Run performs one throttled sweep. Only a List infrastructure failure
// returns an error (the runner retries it, with lastRun left unset so the
// retry is immediate); per-entry delete failures are Warn-logged and never
// fail the sweep, since the runner would retry a failed Run forever.
func (j *gcJob) Run(ctx context.Context, _ []byte) error {
	if j == nil || j.cache == nil || j.prefix == "" {
		// Safety: never sweep without a prefix — that would List/Delete a
		// storage root.
		return nil
	}
	now := j.clock().UTC()
	j.mu.Lock()
	if now.Sub(j.lastRun) < gcMinInterval {
		j.mu.Unlock()
		return nil
	}
	j.mu.Unlock()

	if err := j.sweep(ctx, j.cache, j.prefix, now); err != nil {
		return err
	}
	if j.raw != nil && j.encPrefix != "" {
		if err := j.sweep(ctx, j.raw, j.encPrefix, now); err != nil {
			return err
		}
	}
	j.markRun(now)
	return nil
}

// sweep deletes prefix entries older than maxAge. A missing prefix is an
// empty pass, not an error.
func (j *gcJob) sweep(ctx context.Context, cache storage.Storage, prefix string, now time.Time) error {
	infos, err := cache.List(ctx, prefix)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// No cache directory yet: nothing to sweep.
			return nil
		}
		return err
	}
	cutoff := now.Add(-j.maxAge)
	for _, info := range infos {
		// The cache is flat: leave unexpected subdirectories alone, and
		// keep entries fresh enough to still be reachable.
		if info.IsDir || info.ModTime.After(cutoff) {
			continue
		}
		if derr := cache.Delete(ctx, info.Path); derr != nil && j.logger != nil {
			j.logger.WarnContext(ctx, "preview gc: delete failed", slog.String("path", info.Path), slog.Any("err", derr))
		}
	}
	return nil
}

// markRun records a completed sweep pass for the cadence throttle.
func (j *gcJob) markRun(t time.Time) {
	j.mu.Lock()
	j.lastRun = t
	j.mu.Unlock()
}
