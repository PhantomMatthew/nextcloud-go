package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

// memintrospect_test.go covers ADR-0095: per-plugin memory high-water gauges
// and retrospective runtime.memory_limit_mb enforcement, driven by real
// MemGrowModule guests through Load+Install.

const memProbeID = "com.example.memprobe"

func memProbeManifest() *Manifest {
	m := probeManifest()
	m.Plugin.ID = memProbeID
	return m
}

func memoryHighWater(t *testing.T, reg *observability.Registry) (int64, bool) {
	t.Helper()
	for _, s := range reg.GaugeSeries(observability.MetricPluginMemoryHighWaterBytes) {
		for _, l := range s.Labels {
			if l.Name == "plugin" && l.Value == memProbeID {
				return s.Value, true
			}
		}
	}
	return 0, false
}

func limitExceeded(t *testing.T, reg *observability.Registry) int64 {
	t.Helper()
	var total int64
	for _, s := range reg.CounterSeries(observability.MetricPluginMemoryLimitExceededTotal) {
		for _, l := range s.Labels {
			if l.Name == "plugin" && l.Value == memProbeID {
				total += s.Value
			}
		}
	}
	return total
}

// TestMemoryHighWaterNoLimit: without a manifest limit the grown size lands
// in the high-water gauge and nothing is counted as a breach.
func TestMemoryHighWaterNoLimit(t *testing.T) {
	reg := observability.NewRegistry()
	h, _ := testHost(t, HostConfig{Metrics: reg})
	ctx := context.Background()
	p, err := h.Load(ctx, memProbeManifest(), wasmgen.MemGrowModule(32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	// The module starts at 1 page and grows by 32 (2 MiB) in on_install.
	hw, ok := memoryHighWater(t, reg)
	if !ok {
		t.Fatal("no high-water gauge series for the plugin")
	}
	if want := int64(32) * 64 * 1024; hw < want {
		t.Fatalf("high-water = %d, want >= %d", hw, want)
	}
	if got := limitExceeded(t, reg); got != 0 {
		t.Fatalf("exceeded = %d, want 0 (no limit set)", got)
	}
}

// TestMemoryLimitExceededPooled: 2 MiB grown against a 1 MiB limit — Install
// succeeds (the call's result is not failed), the breach is counted and
// logged, and the destroyed pooled instance is replenished with a fresh one.
func TestMemoryLimitExceededPooled(t *testing.T) {
	reg := observability.NewRegistry()
	h, buf := testHost(t, HostConfig{Metrics: reg})
	ctx := context.Background()
	m := memProbeManifest()
	m.Runtime = RuntimeSection{InstanceModel: "pooled", PoolSize: 1, MemoryLimitMB: 1}
	p, err := h.Load(ctx, m, wasmgen.MemGrowModule(32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatalf("Install must not fail on a retrospective breach: %v", err)
	}
	if got := limitExceeded(t, reg); got != 1 {
		t.Fatalf("exceeded = %d, want 1", got)
	}
	hw, ok := memoryHighWater(t, reg)
	if !ok || hw < int64(32)*64*1024 {
		t.Fatalf("high-water = %d, %v", hw, ok)
	}
	out := buf.String()
	for _, want := range []string{
		"plugins: memory limit exceeded — destroying instance",
		"plugin.id=" + memProbeID,
		"memory_limit_mb=1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q: %q", want, out)
		}
	}
	// The pool was replenished: a subsequent entry call works on the fresh
	// instance, and staying under the limit keeps the count at 1.
	results, err := p.Call(ctx, "ncgo_abi_version")
	if err != nil {
		t.Fatalf("post-breach call = %v", err)
	}
	if len(results) != 1 || results[0] != 1 {
		t.Fatalf("ncgo_abi_version = %v", results)
	}
	if got := limitExceeded(t, reg); got != 1 {
		t.Fatalf("exceeded = %d after quiet call, want 1", got)
	}
}

// TestMemoryLimitExceededSingleton: a breaching singleton is destroyed like
// a trapped one; the next call transparently instantiates a fresh single.
func TestMemoryLimitExceededSingleton(t *testing.T) {
	reg := observability.NewRegistry()
	h, buf := testHost(t, HostConfig{Metrics: reg})
	ctx := context.Background()
	m := memProbeManifest()
	m.Runtime = RuntimeSection{InstanceModel: "singleton", MemoryLimitMB: 1}
	p, err := h.Load(ctx, m, wasmgen.MemGrowModule(32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if got := limitExceeded(t, reg); got != 1 {
		t.Fatalf("exceeded = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "memory limit exceeded") {
		t.Fatalf("missing warn log: %q", buf.String())
	}
	results, err := p.Call(ctx, "ncgo_abi_version")
	if err != nil {
		t.Fatalf("post-breach call = %v", err)
	}
	if len(results) != 1 || results[0] != 1 {
		t.Fatalf("ncgo_abi_version = %v", results)
	}
	if got := limitExceeded(t, reg); got != 1 {
		t.Fatalf("exceeded = %d after quiet call, want 1", got)
	}
}

// TestMemoryIntrospectNilRegistry: metrics off means zero overhead and no
// enforcement — measurement no-ops, so nothing panics and nothing is logged
// even with a limit set.
func TestMemoryIntrospectNilRegistry(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	ctx := context.Background()
	m := memProbeManifest()
	m.Runtime = RuntimeSection{InstanceModel: "pooled", PoolSize: 1, MemoryLimitMB: 1}
	p, err := h.Load(ctx, m, wasmgen.MemGrowModule(32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); strings.Contains(out, "memory limit exceeded") {
		t.Fatalf("nil registry must not log limit warnings: %q", out)
	}
	if _, err := p.Call(ctx, "ncgo_abi_version"); err != nil {
		t.Fatal(err)
	}
}
