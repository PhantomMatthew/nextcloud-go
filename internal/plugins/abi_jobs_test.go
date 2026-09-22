package plugins

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func jobManifest(id string, onJob bool) *Manifest {
	m := probeManifest()
	m.Plugin.ID = id
	m.Capabilities = Capabilities{Jobs: JobsCapabilities{Register: true}}
	if onJob {
		m.EntryPoints.OnJob = "ncgo_on_job"
	}
	return m
}

func installJobModule(t *testing.T, h *Host, m *Manifest, wasm []byte) *Plugin {
	t.Helper()
	p, err := h.Load(context.Background(), m, wasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p
}

func callEnqueue(t *testing.T, p *Plugin) int32 {
	t.Helper()
	results, err := p.Call(context.Background(), "do_enqueue")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("do_enqueue returned nothing")
	}
	return int32(results[0])
}

// syncBuf is a goroutine-safe log sink: the jobs runner delivers from worker
// goroutines while the test asserts on the output.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testHostSync(t *testing.T, cfg HostConfig) (*Host, *syncBuf) {
	t.Helper()
	sb := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(sb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := NewHost(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h, sb
}

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type jobRow struct {
	attempts  int
	lastError string
	completed bool
}

func queryJobRow(t *testing.T, db database.DB) jobRow {
	t.Helper()
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	var (
		r         jobRow
		lastError sql.NullString
		completed sql.NullInt64
	)
	err := std.QueryRowContext(context.Background(),
		"SELECT attempts, last_error, completed_at FROM jobs ORDER BY id LIMIT 1").
		Scan(&r.attempts, &lastError, &completed)
	if err != nil {
		t.Fatal(err)
	}
	r.lastError = lastError.String
	r.completed = completed.Valid
	return r
}

func testJobRunner(t *testing.T) (*jobs.SQLRunner, database.DB) {
	t.Helper()
	db := testDB(t)
	return jobs.NewRunner(jobs.NewSQLStore(db), time.Now, 1, 20*time.Millisecond), db
}

func TestJobEnqueueDenied(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	m := jobManifest("com.example.jobs", true)
	m.Capabilities = Capabilities{} // no jobs.register grant
	p := installJobModule(t, h, m, wasmgen.JobModule("ping", "x", 0))
	if code := callEnqueue(t, p); code != ErrCodePermissionDenied {
		t.Fatalf("code = %d, want %d", code, ErrCodePermissionDenied)
	}
}

func TestJobEnqueueNilRunner(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobModule("ping", "x", 0))
	if code := callEnqueue(t, p); code != ErrCodeUnavailable {
		t.Fatalf("code = %d, want %d", code, ErrCodeUnavailable)
	}
}

func TestJobEnqueueInvalidNames(t *testing.T) {
	jr, _ := testJobRunner(t)
	h, _ := testHost(t, HostConfig{Jobs: jr})
	long := strings.Repeat("a", maxJobNameLen+1)
	for _, tc := range []struct{ name, job string }{
		{"empty", ""},
		{"spaces", "bad name"},
		{"slash", "a/b"},
		{"too-long", long},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobModule(tc.job, "x", 0))
			if code := callEnqueue(t, p); code != ErrCodeInvalidArgument {
				t.Fatalf("code = %d, want %d", code, ErrCodeInvalidArgument)
			}
		})
	}
}

func TestJobEnqueueNoOnJobEntry(t *testing.T) {
	jr, _ := testJobRunner(t)
	h, _ := testHost(t, HostConfig{Jobs: jr})
	p := installJobModule(t, h, jobManifest("com.example.jobs", false), wasmgen.JobModule("ping", "x", 0))
	if code := callEnqueue(t, p); code != ErrCodeInvalidArgument {
		t.Fatalf("code = %d, want %d", code, ErrCodeInvalidArgument)
	}
}

func TestJobEnqueueFarFuture(t *testing.T) {
	jr, _ := testJobRunner(t)
	h, _ := testHost(t, HostConfig{Jobs: jr})
	far := time.Now().Add(maxJobRunAhead + time.Hour).UnixMilli()
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobModule("ping", "x", far))
	if code := callEnqueue(t, p); code != ErrCodeInvalidArgument {
		t.Fatalf("code = %d, want %d", code, ErrCodeInvalidArgument)
	}
}

// errJobRunner fails every Enqueue, simulating a store outage.
type errJobRunner struct{}

func (errJobRunner) Register(jobs.Job) error { return nil }
func (errJobRunner) Unregister(string)       {}
func (errJobRunner) Enqueue(context.Context, string, []byte, time.Time) error {
	return errors.New("store down")
}
func (errJobRunner) Start(context.Context) error { return nil }
func (errJobRunner) Stop(context.Context) error  { return nil }

func TestJobEnqueueRunnerError(t *testing.T) {
	h, buf := testHost(t, HostConfig{Jobs: errJobRunner{}})
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobModule("ping", "x", 0))
	if code := callEnqueue(t, p); code != ErrCodeInternal {
		t.Fatalf("code = %d, want %d", code, ErrCodeInternal)
	}
	if !strings.Contains(buf.String(), "job enqueue failed") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestJobEnqueueOKRowInStore(t *testing.T) {
	jr, db := testJobRunner(t)
	h, _ := testHost(t, HostConfig{Jobs: jr})
	m := jobManifest("com.example.jobs", true)
	p := installJobModule(t, h, m, wasmgen.JobModule("ping", "hello-job", 0))
	if err := h.registerPluginJob(p); err != nil {
		t.Fatal(err)
	}
	if code := callEnqueue(t, p); code != ErrCodeOK {
		t.Fatalf("code = %d, want 0", code)
	}
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	var (
		name    string
		payload []byte
		runAt   int64
	)
	err := std.QueryRowContext(context.Background(), "SELECT name, payload, run_at FROM jobs").Scan(&name, &payload, &runAt)
	if err != nil {
		t.Fatal(err)
	}
	if name != "plugin.com.example.jobs" {
		t.Fatalf("job name = %q, want namespaced", name)
	}
	var env pluginJobEnvelope
	if err := msgpack.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.Name != "ping" || string(env.Payload) != "hello-job" {
		t.Fatalf("envelope = %+v", env)
	}
	if runAt > time.Now().UnixMilli() {
		t.Fatalf("run_at %d in the future for unset run_at", runAt)
	}
}

func TestJobEndToEndDelivered(t *testing.T) {
	jr, _ := testJobRunner(t)
	h, buf := testHostSync(t, HostConfig{Jobs: jr})
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobModule("ping", "hello-job", 0))
	h.attach(p)
	if err := h.registerPluginJob(p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := jr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Stop(ctx) })
	if code := callEnqueue(t, p); code != ErrCodeOK {
		t.Fatalf("code = %d, want 0", code)
	}
	// The guest must see the plugin-local name verbatim, not the namespaced
	// runner job name.
	waitForCond(t, "job delivery", func() bool {
		return strings.Contains(buf.String(), "job ping hello-job")
	})
	if strings.Contains(buf.String(), "plugin.com.example.jobs") {
		t.Fatalf("namespaced job name leaked to guest: %q", buf.String())
	}
}

func TestJobFailureRetriedThenDroppedOnDetach(t *testing.T) {
	jr, db := testJobRunner(t)
	h, _ := testHostSync(t, HostConfig{Jobs: jr})
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobFailListenerModule())
	h.attach(p)
	if err := h.registerPluginJob(p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := jr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Stop(ctx) })
	envelope, err := msgpack.Marshal(pluginJobEnvelope{Name: "boom", Payload: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Enqueue(ctx, "plugin.com.example.jobs", envelope, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "retry after guest failure", func() bool {
		r := queryJobRow(t, db)
		return r.attempts >= 1 && strings.Contains(r.lastError, "plugin error 7") && !r.completed
	})
	// Detaching (disable/uninstall) turns the retry into a drop: Run returns
	// nil and the runner completes the row instead of retrying forever.
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "drop after detach", func() bool {
		return queryJobRow(t, db).completed
	})
}

func TestJobTrapRetried(t *testing.T) {
	jr, db := testJobRunner(t)
	h, _ := testHostSync(t, HostConfig{Jobs: jr})
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobTrapListenerModule())
	h.attach(p)
	if err := h.registerPluginJob(p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := jr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Stop(ctx) })
	envelope, err := msgpack.Marshal(pluginJobEnvelope{Name: "boom", Payload: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Enqueue(ctx, "plugin.com.example.jobs", envelope, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "retry after guest trap", func() bool {
		r := queryJobRow(t, db)
		return r.attempts >= 1 && strings.Contains(r.lastError, "wasm trap") && !r.completed
	})
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "drop after detach", func() bool {
		return queryJobRow(t, db).completed
	})
}

func TestJobMalformedEnvelopeDropped(t *testing.T) {
	jr, db := testJobRunner(t)
	h, buf := testHostSync(t, HostConfig{Jobs: jr})
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobModule("ping", "x", 0))
	h.attach(p)
	if err := h.registerPluginJob(p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := jr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Stop(ctx) })
	if err := jr.Enqueue(ctx, "plugin.com.example.jobs", []byte{0xc1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "poison envelope dropped", func() bool {
		return queryJobRow(t, db).completed
	})
	if !strings.Contains(buf.String(), "malformed job envelope") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestJobRunDetachedDrops(t *testing.T) {
	jr, _ := testJobRunner(t)
	h, buf := testHost(t, HostConfig{Jobs: jr})
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobModule("ping", "x", 0))
	j := &pluginJob{host: h, plugin: p}
	envelope, err := msgpack.Marshal(pluginJobEnvelope{Name: "ping", Payload: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	// Never attached: Run must drop without touching the guest.
	if err := j.Run(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "job ping") {
		t.Fatalf("detached plugin received a job: %q", buf.String())
	}
	// Attached but no on_job entry point: also dropped.
	m := jobManifest("com.example.noentry", false)
	p2 := installJobModule(t, h, m, wasmgen.JobModule("ping", "x", 0))
	h.attach(p2)
	j2 := &pluginJob{host: h, plugin: p2}
	if err := j2.Run(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "job ping") {
		t.Fatalf("entry-less plugin received a job: %q", buf.String())
	}
	if j.Name() != "plugin.com.example.jobs" {
		t.Fatalf("Name = %q", j.Name())
	}
}

func TestJobRegisterIdempotent(t *testing.T) {
	jr, _ := testJobRunner(t)
	h, _ := testHost(t, HostConfig{Jobs: jr})
	p := installJobModule(t, h, jobManifest("com.example.jobs", true), wasmgen.JobModule("ping", "x", 0))
	if err := h.registerPluginJob(p); err != nil {
		t.Fatal(err)
	}
	if err := h.registerPluginJob(p); err != nil {
		t.Fatalf("duplicate registration: %v", err)
	}
	// Enqueueing through the runner proves the adapter registered once.
	envelope, err := msgpack.Marshal(pluginJobEnvelope{Name: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Enqueue(context.Background(), "plugin.com.example.jobs", envelope, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestJobRegisterSkipped(t *testing.T) {
	jr, _ := testJobRunner(t)
	// No runner configured: registration is a no-op.
	hNil, _ := testHost(t, HostConfig{})
	p := installJobModule(t, hNil, jobManifest("com.example.jobs", true), wasmgen.JobModule("ping", "x", 0))
	if err := hNil.registerPluginJob(p); err != nil {
		t.Fatal(err)
	}
	// No capability: skipped.
	h, _ := testHost(t, HostConfig{Jobs: jr})
	m := jobManifest("com.example.jobs", true)
	m.Capabilities = Capabilities{}
	p2 := installJobModule(t, h, m, wasmgen.JobModule("ping", "x", 0))
	if err := h.registerPluginJob(p2); err != nil {
		t.Fatal(err)
	}
	// No on_job entry point: skipped.
	p3 := installJobModule(t, h, jobManifest("com.example.noentry", false), wasmgen.JobModule("ping", "x", 0))
	if err := h.registerPluginJob(p3); err != nil {
		t.Fatal(err)
	}
	err := jr.Enqueue(context.Background(), "plugin.com.example.jobs", nil, time.Now())
	if !errors.Is(err, jobs.ErrUnknownJob) {
		t.Fatalf("enqueue = %v, want ErrUnknownJob", err)
	}
}
