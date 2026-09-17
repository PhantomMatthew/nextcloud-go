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
	Enqueue(ctx context.Context, name string, payload []byte, runAt time.Time) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}
