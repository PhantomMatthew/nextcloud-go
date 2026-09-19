package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type countJob struct {
	name string
	n    *atomic.Int32
	err  error
}

func (c *countJob) Name() string { return c.name }

func (c *countJob) Run(_ context.Context, _ []byte) error {
	c.n.Add(1)
	return c.err
}

func TestRunnerRegisterEnqueueRun(t *testing.T) {
	ctx := t.Context()
	store := NewSQLStore(testDB(t))
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	var clock atomic.Value
	clock.Store(now)
	r := NewRunner(store, func() time.Time { return clock.Load().(time.Time) }, 1, 20*time.Millisecond)
	var n atomic.Int32
	if err := r.Register(&countJob{name: "once", n: &n}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&countJob{name: "once", n: &n}); !errors.Is(err, ErrDuplicateJob) {
		t.Fatalf("dup = %v", err)
	}
	if err := r.Enqueue(ctx, "missing", nil, now); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("unknown enqueue = %v", err)
	}
	if err := r.Enqueue(ctx, "once", nil, now); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(ctx) })
	deadline := time.Now().Add(time.Second)
	for n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n.Load() != 1 {
		t.Fatalf("runs = %d", n.Load())
	}
}

func TestRunnerPeriodicExpireNames(t *testing.T) {
	ctx := t.Context()
	store := NewSQLStore(testDB(t))
	r := NewRunner(store, time.Now, 1, 15*time.Millisecond)
	var n atomic.Int32
	if err := r.Register(&countJob{name: JobSharesExpire, n: &n}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(ctx) })
	deadline := time.Now().Add(time.Second)
	for n.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n.Load() < 1 {
		t.Fatal("periodic job never ran")
	}
}
