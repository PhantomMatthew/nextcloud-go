package cache

import (
	"context"
	"errors"
	"fmt"
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

func TestMemoryDeleteByPrefix(t *testing.T) {
	m, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()

	if _, err := m.DeleteByPrefix(ctx, ""); !errors.Is(err, ErrEmptyPrefix) {
		t.Fatalf("empty prefix = %v", err)
	}
	for _, k := range []string{"plugin:a:x", "plugin:a:y", "plugin:ab:z", "other:k"} {
		if err := m.Set(ctx, k, []byte("v"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Increment(ctx, "plugin:a:n", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Increment(ctx, "other:n", 7); err != nil {
		t.Fatal(err)
	}
	// "plugin:ab:z" must not match the "plugin:a:" prefix boundary.
	n, err := m.DeleteByPrefix(ctx, "plugin:a:")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("deleted = %d want 3", n)
	}
	for _, k := range []string{"plugin:a:x", "plugin:a:y"} {
		if _, err := m.Get(ctx, k); !errors.Is(err, ErrMiss) {
			t.Fatalf("%s after DeleteByPrefix = %v", k, err)
		}
	}
	// A resweep finds nothing: the counter entry was swept from tracking too.
	if n, err := m.DeleteByPrefix(ctx, "plugin:a:"); err != nil || n != 0 {
		t.Fatalf("resweep = %d %v", n, err)
	}
	if v, err := m.Increment(ctx, "plugin:a:n", 1); err != nil || v != 1 {
		t.Fatalf("counter restarted from %d %v", v, err)
	}
	// Non-matching keys and counters survive.
	for _, k := range []string{"plugin:ab:z", "other:k"} {
		if _, err := m.Get(ctx, k); err != nil {
			t.Fatalf("%s lost: %v", k, err)
		}
	}
	if v, err := m.Increment(ctx, "other:n", 0); err != nil || v != 7 {
		t.Fatalf("control counter = %d %v", v, err)
	}
}

// TestMemoryDeleteByPrefixExpiredKey proves a TTL-expired key that the
// cleanup ticker has not reported yet is still swept (a no-op Del) and
// counted.
func TestMemoryDeleteByPrefixExpiredKey(t *testing.T) {
	m, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()
	if err := m.Set(ctx, "ttl:k", []byte("v"), 40*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := m.Get(ctx, "ttl:k"); !errors.Is(err, ErrMiss) {
		t.Fatalf("expired = %v", err)
	}
	n, err := m.DeleteByPrefix(ctx, "ttl:")
	if err != nil || n != 1 {
		t.Fatalf("deleted = %d %v", n, err)
	}
}

// TestMemoryEvictionUntracks hammers a tiny cache past capacity and asserts
// the DeleteByPrefix key index keeps mirroring the store: OnEvict/OnReject
// must untrack every key ristretto drops.
func TestMemoryEvictionUntracks(t *testing.T) {
	m, err := NewMemory(MemoryConfig{MaxItems: 1000, MaxCostBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()
	const total = 50
	for i := range total {
		k := fmt.Sprintf("e:%03d", i)
		if err := m.Set(ctx, k, []byte("v"), 0); err != nil {
			t.Fatal(err)
		}
	}
	hits := 0
	for i := range total {
		k := fmt.Sprintf("e:%03d", i)
		if _, err := m.Get(ctx, k); err == nil {
			hits++
		} else if !errors.Is(err, ErrMiss) {
			t.Fatalf("get %s: %v", k, err)
		}
	}
	m.mu.Lock()
	tracked := len(m.keys)
	m.mu.Unlock()
	if hits >= total {
		t.Fatalf("no eviction happened at MaxCostBytes=512: %d hits", hits)
	}
	if tracked != hits {
		t.Fatalf("key index does not mirror store: tracked %d, stored %d", tracked, hits)
	}
	// Whatever survived is still swept by prefix.
	n, err := m.DeleteByPrefix(ctx, "e:")
	if err != nil || n != int64(hits) {
		t.Fatalf("deleted = %d %v want %d", n, err, hits)
	}
}
