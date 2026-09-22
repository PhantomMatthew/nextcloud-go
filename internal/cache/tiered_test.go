package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTieredFillAndWriteThrough(t *testing.T) {
	l1, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l1.Close)
	l2, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l2.Close)
	ctx := context.Background()
	tiered := NewTiered(l1, l2)
	if err := tiered.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := l2.Get(ctx, "k")
	if err != nil || string(got) != "v" {
		t.Fatalf("l2 write-through = %q %v", got, err)
	}
	if err := l1.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	got, err = tiered.Get(ctx, "k")
	if err != nil || string(got) != "v" {
		t.Fatalf("fill from l2 = %q %v", got, err)
	}
	got, err = l1.Get(ctx, "k")
	if err != nil || string(got) != "v" {
		t.Fatalf("l1 backfill = %q %v", got, err)
	}
	if err := tiered.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := l1.Get(ctx, "k"); !errors.Is(err, ErrMiss) {
		t.Fatalf("l1 after delete = %v", err)
	}
	if _, err := l2.Get(ctx, "k"); !errors.Is(err, ErrMiss) {
		t.Fatalf("l2 after delete = %v", err)
	}
}

func TestTieredIncrementL2Authority(t *testing.T) {
	l1, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l1.Close)
	l2, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l2.Close)
	ctx := context.Background()
	if _, err := l2.Increment(ctx, "n", 10); err != nil {
		t.Fatal(err)
	}
	tiered := NewTiered(l1, l2)
	n, err := tiered.Increment(ctx, "n", 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 12 {
		t.Fatalf("n = %d", n)
	}
	got, err := l2.Increment(ctx, "n", 0)
	if err != nil || got != 12 {
		t.Fatalf("l2 = %d %v", got, err)
	}
}

func TestTieredL1OnlyMiss(t *testing.T) {
	l1, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l1.Close)
	ctx := context.Background()
	tiered := NewTiered(l1, nil)
	if _, err := tiered.Get(ctx, "missing"); !errors.Is(err, ErrMiss) {
		t.Fatalf("err = %v", err)
	}
	n, err := tiered.Increment(ctx, "n", 3)
	if err != nil || n != 3 {
		t.Fatalf("incr = %d %v", n, err)
	}
}

func TestTieredDeleteByPrefix(t *testing.T) {
	l1, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l1.Close)
	l2, err := NewMemory(MemoryConfig{MaxItems: 100, MaxCostBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l2.Close)
	ctx := context.Background()
	tiered := NewTiered(l1, l2)
	for _, k := range []string{"p:a", "p:b", "q:c"} {
		if err := tiered.Set(ctx, k, []byte("v"), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	// The count comes from l2 (the authority), not l1+l2 — the layers
	// mirror the same keyspace.
	n, err := tiered.DeleteByPrefix(ctx, "p:")
	if err != nil || n != 2 {
		t.Fatalf("deletebyprefix = %d %v", n, err)
	}
	for _, k := range []string{"p:a", "p:b"} {
		if _, err := l1.Get(ctx, k); !errors.Is(err, ErrMiss) {
			t.Fatalf("l1 %s survives: %v", k, err)
		}
		if _, err := l2.Get(ctx, k); !errors.Is(err, ErrMiss) {
			t.Fatalf("l2 %s survives: %v", k, err)
		}
	}
	if _, err := l2.Get(ctx, "q:c"); err != nil {
		t.Fatalf("control key lost: %v", err)
	}
	if _, err := tiered.DeleteByPrefix(ctx, ""); !errors.Is(err, ErrEmptyPrefix) {
		t.Fatalf("empty prefix = %v", err)
	}
	// L1-only tiered reports l1's count.
	solo := NewTiered(l1, nil)
	if err := solo.Set(ctx, "s:a", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if n, err := solo.DeleteByPrefix(ctx, "s:"); err != nil || n != 1 {
		t.Fatalf("l1-only deletebyprefix = %d %v", n, err)
	}
}
