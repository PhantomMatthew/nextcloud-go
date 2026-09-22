package plugins

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

func renderMetrics(t *testing.T, reg *observability.Registry) string {
	t.Helper()
	var buf bytes.Buffer
	reg.Render(&buf)
	return buf.String()
}

func TestMetricsHostCallsCounted(t *testing.T) {
	mc, err := cache.NewMemory(cache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	reg := observability.NewRegistry()
	h, _ := testHost(t, HostConfig{Cache: mc, Metrics: reg})
	installModule(t, h, probeManifest(), wasmgen.CacheRoundTripModule("greeting", "hello-cache"))
	out := renderMetrics(t, reg)
	for _, fn := range []string{"cache_set", "cache_get"} {
		want := `ncgo_plugin_host_calls_total{function="` + fn +
			`",plugin="com.example.probe",result="ok"} 1`
		if !strings.Contains(out, want) {
			t.Errorf("missing counter %q:\n%s", want, out)
		}
		want = `ncgo_plugin_host_call_duration_seconds_count{function="` + fn +
			`",plugin="com.example.probe"} 1`
		if !strings.Contains(out, want) {
			t.Errorf("missing histogram count %q:\n%s", want, out)
		}
	}
	// The round-trip module also logs; every host call must be counted.
	if !strings.Contains(out, `ncgo_plugin_host_calls_total{function="log",plugin="com.example.probe",result="ok"}`) {
		t.Errorf("missing log counter:\n%s", out)
	}
	// No denials happened, so the denials family must have no series.
	if strings.Contains(out, "ncgo_plugin_capability_denials_total{") {
		t.Errorf("unexpected denial series:\n%s", out)
	}
}

func TestMetricsCapabilityDenialCounted(t *testing.T) {
	reg := observability.NewRegistry()
	h, _ := testHost(t, HostConfig{Metrics: reg})
	// No events.publish grant: event_publish returns ErrCodePermissionDenied.
	installModule(t, h, probeManifest(), wasmgen.EventProbeModule("core.x", pluginsdk.ErrCodePermissionDenied))
	out := renderMetrics(t, reg)
	want := `ncgo_plugin_host_calls_total{function="event_publish",plugin="com.example.probe",result="permission_denied"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing denial result counter %q:\n%s", want, out)
	}
	want = `ncgo_plugin_capability_denials_total{function="event_publish",plugin="com.example.probe"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing capability denial counter %q:\n%s", want, out)
	}
}

func TestMetricsNilRegistryPassthrough(t *testing.T) {
	// No registry: the export helper must register functions unwrapped, so a
	// full module run works and there is nothing to render.
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, probeManifest(), wasmgen.HelloModule("no-metrics"))
	if !strings.Contains(buf.String(), "no-metrics") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestWrapHostMetricsUnknownPlugin(t *testing.T) {
	reg := observability.NewRegistry()
	h, _ := testHost(t, HostConfig{Metrics: reg})
	// A host-function-shaped dummy called without plugin identity in ctx
	// must be labeled with the "unknown" fallback.
	dummy := func(_ context.Context, _ api.Module, v int32) int32 { return v }
	wrapped, ok := h.wrapHostMetrics("dummy_fn", dummy).(func(context.Context, api.Module, int32) int32)
	if !ok {
		t.Fatal("wrapped function changed signature")
	}
	if got := wrapped(context.Background(), nil, pluginsdk.ErrCodeNotFound); got != pluginsdk.ErrCodeNotFound {
		t.Fatalf("wrapped call returned %d", got)
	}
	out := renderMetrics(t, reg)
	want := `ncgo_plugin_host_calls_total{function="dummy_fn",plugin="unknown",result="not_found"} 1`
	if !strings.Contains(out, want) {
		t.Fatalf("missing unknown-plugin series %q:\n%s", want, out)
	}
}

func TestWrapHostMetricsInt64Unpack(t *testing.T) {
	reg := observability.NewRegistry()
	h, _ := testHost(t, HostConfig{Metrics: reg})
	// int64 results pack the error code in the high 32 bits (packI64).
	dummy := func(_ context.Context, _ api.Module) int64 {
		return packI64(pluginsdk.ErrCodePermissionDenied, 7)
	}
	wrapped, ok := h.wrapHostMetrics("dummy64", dummy).(func(context.Context, api.Module) int64)
	if !ok {
		t.Fatal("wrapped function changed signature")
	}
	ret := wrapped(withPlugin(context.Background(), "com.example.p", "1.0"), nil)
	if ret != packI64(pluginsdk.ErrCodePermissionDenied, 7) {
		t.Fatalf("wrapped call returned %d", ret)
	}
	out := renderMetrics(t, reg)
	want := `ncgo_plugin_host_calls_total{function="dummy64",plugin="com.example.p",result="permission_denied"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing int64 result series %q:\n%s", want, out)
	}
	want = `ncgo_plugin_capability_denials_total{function="dummy64",plugin="com.example.p"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing int64 denial series %q:\n%s", want, out)
	}
}

func TestWrapHostMetricsShapePanic(t *testing.T) {
	reg := observability.NewRegistry()
	h, _ := testHost(t, HostConfig{Metrics: reg})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on non-host-function shape")
		}
	}()
	h.wrapHostMetrics("bad", func(int) int { return 0 })
}
