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
