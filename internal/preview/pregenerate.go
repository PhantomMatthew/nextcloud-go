package preview

import (
	"context"
	"log/slog"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

// pregenerateJob renders the configured hot preview sizes for every uploaded
// file (ADR-0084). It is event-driven: the app wiring enqueues one row per
// files.uploaded event with the event's msgpack payload forwarded verbatim.
type pregenerateJob struct {
	gen    *Generator
	boxes  []int
	logger *slog.Logger
}

// NewPregenerateJob returns the jobs.Job for upload-time preview
// pre-generation. Sizes are clamped to gen.MaxDim (a nil gen clamps to the
// 2048 default), non-positive entries are dropped, and duplicates are
// removed preserving first-seen order.
func NewPregenerateJob(gen *Generator, sizes []int, logger *slog.Logger) jobs.Job {
	maxDim := defaultMaxDim
	if gen != nil {
		maxDim = gen.MaxDim
	}
	seen := make(map[int]struct{}, len(sizes))
	boxes := make([]int, 0, len(sizes))
	for _, s := range sizes {
		if s < 1 {
			continue
		}
		if s > maxDim {
			s = maxDim
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		boxes = append(boxes, s)
	}
	return &pregenerateJob{gen: gen, boxes: boxes, logger: logger}
}

func (j *pregenerateJob) Name() string { return jobs.JobPreviewPregenerate }

// Run decodes one files.uploaded payload and pre-generates the configured
// boxes. Bad payloads and per-file misses return nil: the runner retries a
// failed Run forever, so only infrastructure failures may surface as errors.
func (j *pregenerateJob) Run(ctx context.Context, payload []byte) error {
	if j == nil || j.gen == nil || len(j.boxes) == 0 {
		return nil
	}
	var m map[string]any
	if err := msgpack.Unmarshal(payload, &m); err != nil {
		// A bad payload is not retryable — the runner reschedules a failed
		// Run forever, so garbage rows would poison the queue.
		if j.logger != nil {
			j.logger.WarnContext(ctx, "preview pregeneration: undecodable payload", slog.Any("error", err))
		}
		return nil
	}
	user, ok := m["user"].(string)
	if !ok || user == "" {
		return nil
	}
	path, ok := m["path"].(string)
	if !ok || path == "" {
		return nil
	}
	return j.gen.Pregenerate(ctx, user, path, j.boxes)
}
