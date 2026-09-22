package plugins

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

// reconcileManifest builds the TOML manifest of a route-serving probe
// plugin. jobs adds the jobs.register capability and on_job entry point.
func reconcileManifest(id, version string, jobsCap bool) []byte {
	caps := `
[capabilities.routes]
register = ["/apps/` + id + `/*"]
`
	entry := `on_request = "ncgo_on_request"`
	if jobsCap {
		caps += `
[capabilities.jobs]
register = true
`
		entry += `
on_job = "ncgo_on_job"`
	}
	return []byte(`
[plugin]
id = "` + id + `"
name = "Rec"
version = "` + version + `"
abi = "ncgo-abi/1"
` + caps + `
[entry_points]
module = "hello.wasm"
on_install = "ncgo_on_install"
` + entry + `
`)
}

// writeReconcileArchive stores an unsigned .ncplugin the way the CLI's
// install does: one file per id+version under dir.
func writeReconcileArchive(t *testing.T, dir, id, version string, module []byte) string {
	t.Helper()
	raw := buildArchiveModule(t, reconcileManifest(id, version, false), module, nil)
	path := filepath.Join(dir, id+"-"+version+".ncplugin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func reconcileUpsert(t *testing.T, ctx context.Context, reg *Registry, id, version, archivePath string) {
	t.Helper()
	if err := reg.Upsert(ctx, &RegistryRow{
		ID: id, Version: version, Enabled: true,
		Capabilities: []byte(`{}`), ArchivePath: archivePath,
	}); err != nil {
		t.Fatal(err)
	}
}

func reconcileProbe(t *testing.T, router *httpx.Router, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	return w.Code, w.Body.String()
}

func TestReconcileEnableDisable(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	router := httpx.NewRouter()
	rec := NewReconciler(h, reg, router, nil, nil, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	const id = "com.example.rec"
	// The OCS handler requires a JSON body, so the probe serves one; the
	// plain route then returns it verbatim.
	archive := writeReconcileArchive(t, t.TempDir(), id, "1.0.0", wasmgen.RouteModule(200, nil, `{"ok":true}`))
	reconcileUpsert(t, ctx, reg, id, "1.0.0", archive)
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: id, Kind: "route", Method: "GET", Path: "/apps/" + id + "/api", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}
	// One OCS record mounts twice (v1 + v2); both must come off on disable.
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: id, Kind: "ocs", Method: "GET", Path: "/apps/" + id + "/things", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}

	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/apps/" + id + "/api", "/ocs/v1.php/apps/" + id + "/things", "/ocs/v2.php/apps/" + id + "/things"} {
		if code, _ := reconcileProbe(t, router, path); code != http.StatusOK {
			t.Fatalf("mounted %s: code = %d, log %q", path, code, buf.String())
		}
	}

	// Sync is idempotent: nothing changes when the registry doesn't.
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, body := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("after idempotent Sync: code = %d body = %q", code, body)
	}

	// Disable: the CLI flips the flag; routes stay in the DB. The next Sync
	// unmounts from the tracked keys.
	if err := reg.SetEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/apps/" + id + "/api", "/ocs/v1.php/apps/" + id + "/things", "/ocs/v2.php/apps/" + id + "/things"} {
		if code, _ := reconcileProbe(t, router, path); code != http.StatusNotFound {
			t.Fatalf("disabled %s: code = %d, want 404", path, code)
		}
	}

	// Re-enable: the stop unregistered the job adapter and the start
	// re-registers it, so the restart must not trip ErrDuplicateJob.
	if err := reg.SetEnabled(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, body := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("re-enabled: code = %d body = %q", code, body)
	}
}

func TestReconcileUpgrade(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	router := httpx.NewRouter()
	rec := NewReconciler(h, reg, router, nil, nil, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	const id = "com.example.rec"
	dir := t.TempDir()
	route := &RouteRecord{PluginID: id, Kind: "route", Method: "GET", Path: "/apps/" + id + "/api", HandlerName: "h"}
	if err := reg.UpsertRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	reconcileUpsert(t, ctx, reg, id, "1.0.0",
		writeReconcileArchive(t, dir, id, "1.0.0", wasmgen.RouteModule(200, nil, "v1-body")))
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, body := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusOK || body != "v1-body" {
		t.Fatalf("v1: code = %d body = %q", code, body)
	}

	// The CLI upgrade has already stored the new archive, bumped the
	// registry version, and rewritten the route rows (4j clear-then-hook).
	// A version change is stop-then-start.
	reconcileUpsert(t, ctx, reg, id, "2.0.0",
		writeReconcileArchive(t, dir, id, "2.0.0", wasmgen.RouteModule(200, nil, "v2-body")))
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, body := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusOK || body != "v2-body" {
		t.Fatalf("v2: code = %d body = %q", code, body)
	}
}

func TestReconcileUninstall(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	router := httpx.NewRouter()
	rec := NewReconciler(h, reg, router, nil, nil, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	const id = "com.example.rec"
	reconcileUpsert(t, ctx, reg, id, "1.0.0",
		writeReconcileArchive(t, t.TempDir(), id, "1.0.0", wasmgen.RouteModule(200, nil, "v1-body")))
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: id, Kind: "route", Method: "GET", Path: "/apps/" + id + "/api", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _ := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusOK {
		t.Fatalf("installed: code = %d", code)
	}

	// Uninstall deletes the route rows and the registry row before the
	// server notices — removal must come from the tracked route keys.
	if err := reg.DeleteRoutesForPlugin(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _ := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusNotFound {
		t.Fatalf("uninstalled: code = %d, want 404", code)
	}
}

func TestReconcileBadArchiveIsolated(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	router := httpx.NewRouter()
	// startOne logs through the reconciler's logger; point it at the same
	// buffer so the test can see the isolated failure.
	recLogger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := NewReconciler(h, reg, router, nil, nil, nil, recLogger)
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	dir := t.TempDir()
	// good mounts; broken points at an archive that does not exist yet.
	reconcileUpsert(t, ctx, reg, "com.example.good", "1.0.0",
		writeReconcileArchive(t, dir, "com.example.good", "1.0.0", wasmgen.RouteModule(200, nil, "good")))
	brokenPath := filepath.Join(dir, "com.example.broken-1.0.0.ncplugin")
	reconcileUpsert(t, ctx, reg, "com.example.broken", "1.0.0", brokenPath)
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: "com.example.good", Kind: "route", Method: "GET", Path: "/apps/com.example.good/api", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: "com.example.broken", Kind: "route", Method: "GET", Path: "/apps/com.example.broken/api", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}

	if err := rec.Sync(ctx); err != nil {
		t.Fatalf("per-plugin failure must not fail Sync: %v", err)
	}
	if code, body := reconcileProbe(t, router, "/apps/com.example.good/api"); code != http.StatusOK || body != "good" {
		t.Fatalf("good plugin: code = %d body = %q", code, body)
	}
	if code, _ := reconcileProbe(t, router, "/apps/com.example.broken/api"); code != http.StatusNotFound {
		t.Fatalf("broken plugin must not mount: code = %d", code)
	}
	if !strings.Contains(buf.String(), "com.example.broken") {
		t.Fatalf("broken plugin not logged: %q", buf.String())
	}

	// Not tracked means retried: once the archive lands, the next Sync
	// starts it.
	raw := buildArchiveModule(t, reconcileManifest("com.example.broken", "1.0.0", false), wasmgen.RouteModule(200, nil, "fixed"), nil)
	if err := os.WriteFile(brokenPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, body := reconcileProbe(t, router, "/apps/com.example.good/api"); code != http.StatusOK || body != "good" {
		t.Fatalf("good plugin after retry: code = %d body = %q", code, body)
	}
	if code, body := reconcileProbe(t, router, "/apps/com.example.broken/api"); code != http.StatusOK || body != "fixed" {
		t.Fatalf("broken plugin after archive landed: code = %d body = %q", code, body)
	}
}

func TestReconcileJobAdapterLifecycle(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	reg := NewRegistry(db)
	jr := jobs.NewRunner(jobs.NewSQLStore(db), time.Now, 1, 20*time.Millisecond)
	h, _ := testHost(t, HostConfig{Registry: reg, Jobs: jr})
	router := httpx.NewRouter()
	rec := NewReconciler(h, reg, router, nil, nil, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	const id = "com.example.jobrec"
	const jobName = "plugin." + id
	dir := t.TempDir()
	manifest := reconcileManifest(id, "1.0.0", true)
	raw := buildArchiveModule(t, manifest, wasmgen.JobModule("ping", "x", 0), nil)
	archivePath := filepath.Join(dir, id+"-1.0.0.ncplugin")
	if err := os.WriteFile(archivePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reconcileUpsert(t, ctx, reg, id, "1.0.0", archivePath)

	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := jr.Enqueue(ctx, jobName, nil, time.Now()); err != nil {
		t.Fatalf("enqueue while running = %v", err)
	}

	// Disable: Sync unregisters the adapter, so the name goes unknown...
	if err := reg.SetEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := jr.Enqueue(ctx, jobName, nil, time.Now()); !errors.Is(err, jobs.ErrUnknownJob) {
		t.Fatalf("enqueue while disabled = %v", err)
	}

	// ...and re-enabling re-registers it (no ErrDuplicateJob from the
	// pre-4j stale-adapter shape).
	if err := reg.SetEnabled(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := jr.Enqueue(ctx, jobName, nil, time.Now()); err != nil {
		t.Fatalf("enqueue after re-enable = %v", err)
	}
}

func TestReconcileRegistryErrorKeepsState(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	reg := NewRegistry(db)
	h, _ := testHost(t, HostConfig{Registry: reg})
	router := httpx.NewRouter()
	rec := NewReconciler(h, reg, router, nil, nil, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	const id = "com.example.rec"
	reconcileUpsert(t, ctx, reg, id, "1.0.0",
		writeReconcileArchive(t, t.TempDir(), id, "1.0.0", wasmgen.RouteModule(200, nil, "v1-body")))
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: id, Kind: "route", Method: "GET", Path: "/apps/" + id + "/api", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// A registry outage aborts the pass with an error and keeps the
	// currently running set untouched.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err == nil {
		t.Fatal("Sync must report the registry read failure")
	}
	if code, body := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusOK || body != "v1-body" {
		t.Fatalf("state kept during outage: code = %d body = %q", code, body)
	}
}

func TestReconcilerCloseStopsEverything(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	router := httpx.NewRouter()
	rec := NewReconciler(h, reg, router, nil, nil, nil, slog.New(slog.DiscardHandler))

	const id = "com.example.rec"
	reconcileUpsert(t, ctx, reg, id, "1.0.0",
		writeReconcileArchive(t, t.TempDir(), id, "1.0.0", wasmgen.RouteModule(200, nil, "v1-body")))
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: id, Kind: "route", Method: "GET", Path: "/apps/" + id + "/api", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _ := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusOK {
		t.Fatalf("running: code = %d", code)
	}

	if err := rec.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _ := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusNotFound {
		t.Fatalf("after Close: code = %d, want 404", code)
	}
	// Sync after Close is a no-op even if the registry says enabled.
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _ := reconcileProbe(t, router, "/apps/"+id+"/api"); code != http.StatusNotFound {
		t.Fatalf("Sync after Close must not remount: code = %d", code)
	}
	if err := rec.Close(ctx); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// TestReconcilerRunPolling drives the full hot path: Run notices a disable
// made by another "process" (the test) within one interval.
func TestReconcilerRunPolling(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	router := httpx.NewRouter()
	rec := NewReconciler(h, reg, router, nil, nil, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	const id = "com.example.rec"
	reconcileUpsert(t, ctx, reg, id, "1.0.0",
		writeReconcileArchive(t, t.TempDir(), id, "1.0.0", wasmgen.RouteModule(200, nil, "v1-body")))
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: id, Kind: "route", Method: "GET", Path: "/apps/" + id + "/api", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// interval <= 0 disables polling and returns immediately.
	rec.Run(ctx, 0)

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rec.Run(runCtx, 10*time.Millisecond)
		close(done)
	}()
	if err := reg.SetEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "route unmounted by poll", func() bool {
		code, _ := reconcileProbe(t, router, "/apps/"+id+"/api")
		return code == http.StatusNotFound
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}
}
