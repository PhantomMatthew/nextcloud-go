package plugins

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func testHost(t *testing.T, cfg HostConfig) (*Host, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := NewHost(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h, &buf
}

func probeManifest() *Manifest {
	return &Manifest{
		Plugin:      PluginSection{ID: "com.example.probe", Name: "Probe", Version: "0.1.0", ABI: abiV1},
		EntryPoints: EntryPointsSection{Module: "probe.wasm", OnInstall: "ncgo_on_install"},
	}
}

func installModuleCtx(t *testing.T, ctx context.Context, h *Host, m *Manifest, wasm []byte) {
	t.Helper()
	p, err := h.Load(ctx, m, wasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
}

func installModule(t *testing.T, h *Host, m *Manifest, wasm []byte) {
	t.Helper()
	installModuleCtx(t, context.Background(), h, m, wasm)
}

func TestCtxUserIDLogged(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	ctx := WithCallContext(context.Background(), CallContext{UserID: "alice", RequestID: "req-1"})
	installModuleCtx(t, ctx, h, probeManifest(), wasmgen.CtxUserModule())
	if !strings.Contains(buf.String(), "alice") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestCacheRoundTrip(t *testing.T) {
	mc, err := cache.NewMemory(cache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, HostConfig{Cache: mc})
	installModule(t, h, probeManifest(), wasmgen.CacheRoundTripModule("greeting", "hello-cache"))
	if !strings.Contains(buf.String(), "hello-cache") {
		t.Fatalf("log %q", buf.String())
	}
	// The key must be namespaced under the plugin id.
	if _, err := mc.Get(context.Background(), "plugin:com.example.probe:greeting"); err != nil {
		t.Fatalf("namespaced key missing: %v", err)
	}
}

func TestCacheUnavailable(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p, err := h.Load(ctx, probeManifest(), wasmgen.CacheRoundTripModule("k", "v"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	err = p.Install(ctx)
	var pe *PluginError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
}

func TestCacheIncrement(t *testing.T) {
	mc, err := cache.NewMemory(cache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, HostConfig{Cache: mc})
	installModule(t, h, probeManifest(), wasmgen.CacheIncrementModule("counter", 5))
	if !strings.Contains(buf.String(), "incr-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestCryptoHash(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, probeManifest(), wasmgen.CryptoHashModule("abc"))
	if !strings.Contains(buf.String(), "hash-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestEventPublishDenied(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, probeManifest(), wasmgen.EventProbeModule("files.uploaded", ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestEventPublishGrantedUnavailable(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	m := probeManifest()
	m.Capabilities = Capabilities{Events: EventsCapabilities{Publish: []string{"files.*"}}}
	installModule(t, h, m, wasmgen.EventProbeModule("files.uploaded", ErrCodeUnavailable))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestRouteRegisterDenied(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, probeManifest(), wasmgen.RouteProbeModule("/apps/tagger/api", ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestCallTimeout(t *testing.T) {
	h, _ := testHost(t, HostConfig{DefaultCallTimeout: 50 * time.Millisecond})
	ctx := context.Background()
	p, err := h.Load(ctx, probeManifest(), wasmgen.LoopModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	start := time.Now()
	err = p.Install(ctx)
	if !errors.Is(err, ErrTrap) {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("elapsed = %s", elapsed)
	}
}
