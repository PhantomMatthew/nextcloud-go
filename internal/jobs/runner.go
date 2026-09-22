package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	JobSharesExpire = "shares.expire"
	JobLocksExpire  = "locks.expire"
)

// maxUnknownJobAttempts bounds how many times a row whose name no runner
// job knows (e.g. a plugin's leftover plugin.<id> rows after uninstall) is
// failed and rescheduled before the runner drops it as undeliverable.
// Without the cap such rows were retried on every poll forever.
const maxUnknownJobAttempts = 3

// SQLRunner is a single-node Runner over Store.
type SQLRunner struct {
	store    Store
	clock    func() time.Time
	workers  int
	poll     time.Duration
	mu       sync.Mutex
	jobs     map[string]Job
	periodic map[string]struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	started  bool

	// Logger, when set, receives the warn emitted when an undeliverable
	// (unknown-name) row is dropped after maxUnknownJobAttempts. Nil means
	// the drop is silent.
	Logger *slog.Logger
}

// NewRunner returns a SQLRunner.
func NewRunner(store Store, clock func() time.Time, workers int, poll time.Duration) *SQLRunner {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if workers <= 0 {
		workers = 1
	}
	if poll <= 0 {
		poll = 5 * time.Second
	}
	return &SQLRunner{
		store:    store,
		clock:    clock,
		workers:  workers,
		poll:     poll,
		jobs:     map[string]Job{},
		periodic: map[string]struct{}{JobSharesExpire: {}, JobLocksExpire: {}},
	}
}

func (r *SQLRunner) now() time.Time {
	return r.clock().UTC()
}

func (r *SQLRunner) Register(job Job) error {
	if r == nil || job == nil || job.Name() == "" {
		return fmt.Errorf("jobs: invalid job")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.jobs[job.Name()]; ok {
		return ErrDuplicateJob
	}
	r.jobs[job.Name()] = job
	return nil
}

func (r *SQLRunner) Enqueue(ctx context.Context, name string, payload []byte, runAt time.Time) error {
	r.mu.Lock()
	_, ok := r.jobs[name]
	r.mu.Unlock()
	if !ok {
		return ErrUnknownJob
	}
	row := &Row{
		Name:      name,
		Payload:   payload,
		RunAt:     runAt.UTC().UnixMilli(),
		CreatedAt: r.now().UnixMilli(),
	}
	return r.store.Insert(ctx, row)
}

func (r *SQLRunner) Start(ctx context.Context) error {
	if r == nil || r.store == nil {
		return fmt.Errorf("jobs: nil runner")
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = true
	loopCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	for _, name := range []string{JobSharesExpire, JobLocksExpire} {
		r.mu.Lock()
		_, ok := r.jobs[name]
		r.mu.Unlock()
		if !ok {
			continue
		}
		if err := r.Enqueue(ctx, name, nil, r.now().Add(r.poll)); err != nil {
			cancel()
			r.mu.Lock()
			r.started = false
			r.cancel = nil
			r.mu.Unlock()
			return err
		}
	}

	ch := make(chan Row, r.workers)
	for range r.workers {
		r.wg.Add(1)
		go r.worker(loopCtx, ch)
	}
	r.wg.Add(1)
	go r.pollLoop(loopCtx, ch)
	return nil
}

func (r *SQLRunner) Stop(_ context.Context) error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	cancel := r.cancel
	r.started = false
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.wg.Wait()
	return nil
}

func (r *SQLRunner) pollLoop(ctx context.Context, ch chan Row) {
	defer r.wg.Done()
	defer close(ch)
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()
	r.claim(ctx, ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.claim(ctx, ch)
		}
	}
}

func (r *SQLRunner) claim(ctx context.Context, ch chan Row) {
	rows, err := r.store.ClaimDue(ctx, r.now(), r.workers)
	if err != nil {
		return
	}
	for _, row := range rows {
		select {
		case <-ctx.Done():
			return
		case ch <- row:
		}
	}
}

func (r *SQLRunner) worker(ctx context.Context, ch chan Row) {
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case row, ok := <-ch:
			if !ok {
				return
			}
			r.runOne(ctx, row)
		}
	}
}

func (r *SQLRunner) runOne(ctx context.Context, row Row) {
	r.mu.Lock()
	job := r.jobs[row.Name]
	_, periodic := r.periodic[row.Name]
	r.mu.Unlock()
	if job == nil {
		if row.Attempts >= maxUnknownJobAttempts {
			if r.Logger != nil {
				r.Logger.WarnContext(ctx, "jobs: dropping undeliverable row",
					slog.String("job", row.Name), slog.Int64("id", row.ID), slog.Int("attempts", row.Attempts))
			}
			if err := r.store.Complete(ctx, row.ID); err != nil {
				return
			}
			return
		}
		if err := r.store.Fail(ctx, row.ID, ErrUnknownJob.Error(), r.now().Add(r.poll)); err != nil {
			return
		}
		return
	}
	if err := job.Run(ctx, row.Payload); err != nil {
		if ferr := r.store.Fail(ctx, row.ID, err.Error(), r.now().Add(r.poll)); ferr != nil {
			return
		}
		return
	}
	if err := r.store.Complete(ctx, row.ID); err != nil {
		return
	}
	if periodic {
		if err := r.Enqueue(ctx, row.Name, nil, r.now().Add(r.poll)); err != nil {
			return
		}
	}
}
