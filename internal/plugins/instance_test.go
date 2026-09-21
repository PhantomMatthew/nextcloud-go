package plugins

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func counterManifest(model string, poolSize int) *Manifest {
	return &Manifest{
		Plugin:      PluginSection{ID: "com.example.counter", Name: "Counter", Version: "0.1.0", ABI: abiV1},
		Runtime:     RuntimeSection{InstanceModel: model, PoolSize: poolSize},
		EntryPoints: EntryPointsSection{Module: "counter.wasm"},
	}
}

func bump(t *testing.T, p *Plugin) int64 {
	t.Helper()
	results, err := p.Call(context.Background(), "bump")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %v", results)
	}
	return int64(results[0])
}

func TestPerRequestFreshInstances(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	p, err := h.Load(ctx, counterManifest("per_request", 0), wasmgen.CounterModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	// Every call gets a fresh instance, so the counter restarts at 1.
	for i := 0; i < 3; i++ {
		if got := bump(t, p); got != 1 {
			t.Fatalf("bump = %d, want 1", got)
		}
	}
}

func TestSingletonSharedInstance(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	p, err := h.Load(ctx, counterManifest("singleton", 0), wasmgen.CounterModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	for want := int64(1); want <= 3; want++ {
		if got := bump(t, p); got != want {
			t.Fatalf("bump = %d, want %d", got, want)
		}
	}
}

func TestPooledConcurrentCalls(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	p, err := h.Load(ctx, counterManifest("pooled", 2), wasmgen.CounterModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Call(ctx, "bump"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestPooledTrapReplenishes(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{DefaultCallTimeout: 50 * time.Millisecond}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	m := &Manifest{
		Plugin:      PluginSection{ID: "com.example.looper", Name: "Looper", Version: "0.1.0", ABI: abiV1},
		Runtime:     RuntimeSection{InstanceModel: "pooled", PoolSize: 1},
		EntryPoints: EntryPointsSection{Module: "loop.wasm"},
	}
	p, err := h.Load(ctx, m, wasmgen.LoopModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	// Install warms the pool (no on_install hook is set).
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	// Two sequential calls both trap; the pool must replenish the destroyed
	// instance so the second call is not stuck behind a missing instance.
	for i := 0; i < 2; i++ {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := p.Call(callCtx, "ncgo_on_install")
		cancel()
		if !errors.Is(err, ErrTrap) {
			t.Fatalf("call %d: err = %v", i, err)
		}
	}
}

func TestHandleTable(t *testing.T) {
	ht := newHandleTable()
	var firstRowsID int32
	for i := int32(0); i < maxRowsHandles; i++ {
		id, err := ht.add(handleRows, i)
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
		if i == 0 {
			firstRowsID = id
		}
	}
	if _, err := ht.add(handleRows, nil); !errors.Is(err, ErrHandleLimit) {
		t.Fatalf("err = %v", err)
	}
	// Streams have their own budget.
	if _, err := ht.add(handleStream, nil); err != nil {
		t.Fatal(err)
	}
	// Wrong-kind lookups are rejected.
	id, err := ht.add(handleStream, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ht.get(id, handleRows); ok {
		t.Fatal("cross-kind get succeeded")
	}
	got, ok := ht.get(id, handleStream)
	if !ok || got != "s" {
		t.Fatalf("get = %v, %v", got, ok)
	}
	v, ok := ht.remove(id, handleStream)
	if !ok || v != "s" {
		t.Fatalf("remove = %v, %v", v, ok)
	}
	if _, ok := ht.get(id, handleStream); ok {
		t.Fatal("get after remove succeeded")
	}
	// A freed rows slot can be reused.
	if _, ok := ht.remove(firstRowsID, handleRows); !ok {
		t.Fatal("remove rows failed")
	}
	if _, err := ht.add(handleRows, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallRunsHook(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	m := counterManifest("per_request", 0)
	m.EntryPoints.OnUninstall = "ncgo_on_install" // reuse the trivial export
	p, err := h.Load(ctx, m, wasmgen.CounterModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
}
