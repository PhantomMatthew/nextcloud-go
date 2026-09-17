package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestMemoryGetSetDelete(t *testing.T) {
	m, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()
	if _, err := m.Get(ctx, "k"); !errors.Is(err, ErrMiss) {
		t.Fatalf("miss = %v", err)
	}
	if err := m.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v" {
		t.Fatalf("got %q", got)
	}
	if err := m.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "k"); !errors.Is(err, ErrMiss) {
		t.Fatalf("after delete = %v", err)
	}
}

func TestMemoryTTL(t *testing.T) {
	m, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()
	if err := m.Set(ctx, "k", []byte("v"), 40*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := m.Get(ctx, "k"); !errors.Is(err, ErrMiss) {
		t.Fatalf("expired = %v", err)
	}
}

func TestMemoryIncrementConcurrent(t *testing.T) {
	m, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()
	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			if _, err := m.Increment(ctx, "n", 1); err != nil {
				t.Errorf("incr: %v", err)
			}
		}()
	}
	wg.Wait()
	n, err := m.Increment(ctx, "n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != workers {
		t.Fatalf("n = %d want %d", n, workers)
	}
}
