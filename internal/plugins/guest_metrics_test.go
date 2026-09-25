package plugins

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

// loadGuestMetricsPlugin loads wasm with a metrics registry wired into the
// host and installs it, so assertions can also cover the lifecycle calls
// Start/Install issue on their own (ncgo_abi_version, ncgo_on_install).
func loadGuestMetricsPlugin(t *testing.T, cfg HostConfig, wasm []byte) (*Plugin, *observability.Registry) {
	t.Helper()
	reg := observability.NewRegistry()
	cfg.Metrics = reg
	h, _ := testHost(t, cfg)
	ctx := context.Background()
	p, err := h.Load(ctx, probeManifest(), wasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	return p, reg
}

func TestGuestCallMetricsOK(t *testing.T) {
	p, reg := loadGuestMetricsPlugin(t, HostConfig{}, wasmgen.CounterModule())
	results, err := p.Call(context.Background(), "bump")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0] != 1 {
		t.Fatalf("bump = %v", results)
	}
	out := renderMetrics(t, reg)
	// The direct Call is counted...
	want := `ncgo_plugin_entry_calls_total{entry="bump",plugin="com.example.probe",result="ok"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing entry counter %q:\n%s", want, out)
	}
	want = `ncgo_plugin_entry_call_duration_seconds_count{entry="bump",plugin="com.example.probe"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing entry histogram count %q:\n%s", want, out)
	}
	// ...and so are the lifecycle calls Install funnelled through call:
	// Start's ABI check and the on_install hook.
	for _, entry := range []string{"ncgo_abi_version", "ncgo_on_install"} {
		want = `ncgo_plugin_entry_calls_total{entry="` + entry +
			`",plugin="com.example.probe",result="ok"} 1`
		if !strings.Contains(out, want) {
			t.Errorf("missing lifecycle counter %q:\n%s", want, out)
		}
	}
}

func TestGuestCallMetricsMissingExport(t *testing.T) {
	p, reg := loadGuestMetricsPlugin(t, HostConfig{}, wasmgen.CounterModule())
	_, err := p.Call(context.Background(), "no_such_export")
	if !errors.Is(err, ErrMissingExport) {
		t.Fatalf("err = %v", err)
	}
	out := renderMetrics(t, reg)
	want := `ncgo_plugin_entry_calls_total{entry="no_such_export",plugin="com.example.probe",result="missing_export"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing missing_export counter %q:\n%s", want, out)
	}
	want = `ncgo_plugin_entry_call_duration_seconds_count{entry="no_such_export",plugin="com.example.probe"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing missing_export histogram count %q:\n%s", want, out)
	}
}

func TestGuestCallMetricsTrap(t *testing.T) {
	p, reg := loadGuestMetricsPlugin(t, HostConfig{}, wasmgen.EventTrapListenerModule())
	_, err := p.Call(context.Background(), "ncgo_on_event", 0, 0, 0, 0)
	if !errors.Is(err, ErrTrap) {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("plain trap must not look like a timeout: %v", err)
	}
	out := renderMetrics(t, reg)
	want := `ncgo_plugin_entry_calls_total{entry="ncgo_on_event",plugin="com.example.probe",result="trap"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing trap counter %q:\n%s", want, out)
	}
}

func TestGuestCallMetricsTimeout(t *testing.T) {
	reg := observability.NewRegistry()
	h, _ := testHost(t, HostConfig{DefaultCallTimeout: 50 * time.Millisecond, Metrics: reg})
	ctx := context.Background()
	p, err := h.Load(ctx, probeManifest(), wasmgen.LoopModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	// LoopModule spins forever in ncgo_on_install; invoke it directly so the
	// per-call deadline fires.
	_, err = p.Call(ctx, "ncgo_on_install")
	// wrapTrap double-wraps with %w: the error matches both ErrTrap and
	// context.DeadlineExceeded, and the classifier must prefer timeout.
	if !errors.Is(err, ErrTrap) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	out := renderMetrics(t, reg)
	want := `ncgo_plugin_entry_calls_total{entry="ncgo_on_install",plugin="com.example.probe",result="timeout"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing timeout counter %q:\n%s", want, out)
	}
	if strings.Contains(out, `entry="ncgo_on_install",plugin="com.example.probe",result="trap"`) {
		t.Errorf("timeout must not classify as trap:\n%s", out)
	}
}

func TestGuestCallMetricsNilRegistry(t *testing.T) {
	// No registry: Call takes the uninstrumented path and records nothing.
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p, err := h.Load(ctx, probeManifest(), wasmgen.CounterModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := p.Call(ctx, "bump")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0] != 1 {
		t.Fatalf("bump = %v", results)
	}
}
