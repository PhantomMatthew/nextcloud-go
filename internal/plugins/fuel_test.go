package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

// fuel_test.go covers ADR-0103: runtime.fuel_per_call enforced as a wasm
// function-entry budget per plugin call, driven by the wasmgen FuelModule
// probe (recurse for guest entries, spam for host-function entries).

const fuelProbeID = "com.example.fuelprobe"

func fuelManifest(model string, poolSize int, fuel uint64) *Manifest {
	return &Manifest{
		Plugin:      PluginSection{ID: fuelProbeID, Name: "FuelProbe", Version: "0.1.0", ABI: abiV1},
		Runtime:     RuntimeSection{InstanceModel: model, PoolSize: poolSize, FuelPerCall: fuel},
		EntryPoints: EntryPointsSection{Module: "fuel.wasm"},
	}
}

func fuelKills(t *testing.T, reg *observability.Registry) int64 {
	t.Helper()
	var total int64
	for _, s := range reg.CounterSeries(observability.MetricPluginFuelExceededTotal) {
		for _, l := range s.Labels {
			if l.Name == "plugin" && l.Value == fuelProbeID {
				total += s.Value
			}
		}
	}
	return total
}

func loadFuel(t *testing.T, h *Host, m *Manifest) *Plugin {
	t.Helper()
	p, err := h.Load(context.Background(), m, wasmgen.FuelModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

// TestFuelZeroBudgetUnmetered: fuel_per_call unset means exactly the
// unmetered path — no meter on the instance, nothing in the host's meter
// map, and a high-entry call succeeds.
func TestFuelZeroBudgetUnmetered(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("per_request", 0, 0))
	inst, release, err := p.manager.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inst.fuel != nil {
		t.Fatal("fuel=0 instance carries a meter")
	}
	release(false)
	if len(h.fuelMeters) != 0 {
		t.Fatalf("fuelMeters = %d entries, want 0", len(h.fuelMeters))
	}
	if _, err := p.Call(ctx, "recurse", 100000); err != nil {
		t.Fatalf("unmetered high-entry call = %v", err)
	}
}

// TestFuelBudgetExceeded: a small budget kills the call with
// ErrFuelExhausted (not ErrTrap), and the message carries plugin id, entry,
// and budget.
func TestFuelBudgetExceeded(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("per_request", 0, 50))
	_, err := p.Call(ctx, "recurse", 100)
	if !errors.Is(err, ErrFuelExhausted) {
		t.Fatalf("err = %v, want ErrFuelExhausted", err)
	}
	if errors.Is(err, ErrTrap) {
		t.Fatalf("fuel kill must not wrap ErrTrap: %v", err)
	}
	for _, want := range []string{fuelProbeID, "recurse", "50"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q missing %q", err.Error(), want)
		}
	}
}

// TestFuelKillStopsCallPromptly: the kill aborts the in-flight call at
// function granularity — recurse(1e6) with budget 50 returns immediately,
// not after the recursion runs its course.
func TestFuelKillStopsCallPromptly(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("per_request", 0, 50))
	start := time.Now()
	_, err := p.Call(ctx, "recurse", 1000000)
	el := time.Since(start)
	if !errors.Is(err, ErrFuelExhausted) {
		t.Fatalf("err = %v, want ErrFuelExhausted", err)
	}
	if el > 5*time.Second {
		t.Fatalf("killed call took %v — not stopped at the budget", el)
	}
}

// TestFuelPooledInstanceDestroyedAndReplenished: a fuel kill destroys the
// pooled instance like a trap; the pool replenishes with a fresh instance
// and the next call succeeds on it.
func TestFuelPooledInstanceDestroyedAndReplenished(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("pooled", 1, 50))
	if err := p.Install(ctx); err != nil { // warms the pool
		t.Fatal(err)
	}
	instA, release, err := p.manager.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release(false)
	if _, err := p.Call(ctx, "recurse", 100); !errors.Is(err, ErrFuelExhausted) {
		t.Fatalf("kill call = %v", err)
	}
	instB, release2, err := p.manager.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if instA == instB {
		release2(false)
		t.Fatal("pool handed back the killed instance")
	}
	release2(false)
	if _, err := p.Call(ctx, "recurse", 10); err != nil {
		t.Fatalf("post-kill call on fresh instance = %v", err)
	}
}

// TestFuelSingletonReinstantiates: a fuel-killed singleton is destroyed and
// the next call transparently instantiates a fresh single.
func TestFuelSingletonReinstantiates(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("singleton", 0, 50))
	if _, err := p.Call(ctx, "recurse", 10); err != nil {
		t.Fatal(err)
	}
	first := p.manager.single
	if first == nil {
		t.Fatal("no singleton after first call")
	}
	if _, err := p.Call(ctx, "recurse", 100); !errors.Is(err, ErrFuelExhausted) {
		t.Fatalf("kill call = %v", err)
	}
	if p.manager.single != nil {
		t.Fatal("killed singleton not cleared")
	}
	if _, err := p.Call(ctx, "recurse", 10); err != nil {
		t.Fatalf("post-kill call = %v", err)
	}
	if p.manager.single == nil || p.manager.single == first {
		t.Fatal("singleton not re-instantiated")
	}
}

// TestFuelMeterResetsPerCall: consecutive calls on the same reused instance
// each get the full budget — recurse(9) is exactly 10 function entries, at
// a budget of 10, and must succeed twice in a row.
func TestFuelMeterResetsPerCall(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("singleton", 0, 10))
	for i := 0; i < 2; i++ {
		if _, err := p.Call(ctx, "recurse", 9); err != nil {
			t.Fatalf("call %d at the budget boundary = %v", i, err)
		}
	}
}

// TestFuelMetricCountsKills: every fuel kill increments
// ncgo_plugin_fuel_exceeded_total{plugin}; successful calls do not.
func TestFuelMetricCountsKills(t *testing.T) {
	reg := observability.NewRegistry()
	h, _ := testHost(t, HostConfig{Metrics: reg})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("per_request", 0, 50))
	for i := 0; i < 2; i++ {
		if _, err := p.Call(ctx, "recurse", 100); !errors.Is(err, ErrFuelExhausted) {
			t.Fatalf("kill %d = %v", i, err)
		}
	}
	if _, err := p.Call(ctx, "recurse", 10); err != nil {
		t.Fatal(err)
	}
	if got := fuelKills(t, reg); got != 2 {
		t.Fatalf("fuel kills = %d, want 2", got)
	}
}

// TestFuelNilMetricsKillsCleanly: without a registry the kill path is
// unaffected — same error, no metric, no panic.
func TestFuelNilMetricsKillsCleanly(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("per_request", 0, 50))
	if _, err := p.Call(ctx, "recurse", 100); !errors.Is(err, ErrFuelExhausted) {
		t.Fatalf("err = %v, want ErrFuelExhausted", err)
	}
}

// TestFuelHostEntriesCounted: ncgo host-function entries count toward the
// budget too — spam(n) is 1 guest entry plus n log host entries, so a
// budget of 5 allows spam(4) and kills spam(5).
func TestFuelHostEntriesCounted(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("per_request", 0, 5))
	if _, err := p.Call(ctx, "spam", 4); err != nil {
		t.Fatalf("spam(4) at the boundary = %v", err)
	}
	if _, err := p.Call(ctx, "spam", 5); !errors.Is(err, ErrFuelExhausted) {
		t.Fatalf("spam(5) = %v, want ErrFuelExhausted", err)
	}
}

// TestFuelPooledConcurrentCalls: concurrent calls on a pooled plugin keep
// their own budget accounting — under-budget calls succeed, over-budget
// calls die with ErrFuelExhausted, no cross-talk and no data race (-race).
func TestFuelPooledConcurrentCalls(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	ctx := context.Background()
	p := loadFuel(t, h, fuelManifest("pooled", 2, 1000))
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if _, err := p.Call(ctx, "recurse", 50); err != nil {
					errs <- fmt.Errorf("under-budget call: %w", err)
				}
				return
			}
			if _, err := p.Call(ctx, "recurse", 5000); !errors.Is(err, ErrFuelExhausted) {
				errs <- fmt.Errorf("over-budget call = %w, want ErrFuelExhausted", err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
