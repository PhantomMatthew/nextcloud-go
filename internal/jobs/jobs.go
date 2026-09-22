package jobs

import (
	"context"
	"errors"
	"time"
)

var (
	ErrDuplicateJob = errors.New("jobs: duplicate job name")
	ErrUnknownJob   = errors.New("jobs: unknown job")
)

// Job is a named background task.
type Job interface {
	Name() string
	Run(ctx context.Context, payload []byte) error
}

// Runner registers and executes jobs with at-least-once semantics.
type Runner interface {
	Register(job Job) error
	// Unregister drops the job registered under name (plugin hot-reload,
	// ADR-0062: a stopped plugin's adapter must not keep dispatching to a
	// dead *Plugin). Unknown names are a silent no-op; rows already queued
	// under name fall to the runner's unknown-job retry/drop path.
	Unregister(name string)
	Enqueue(ctx context.Context, name string, payload []byte, runAt time.Time) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}
