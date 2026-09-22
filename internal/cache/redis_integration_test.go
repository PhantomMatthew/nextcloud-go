//go:build integration

package cache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestIntegrationRedis(t *testing.T) {
	addr := os.Getenv("NCGO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NCGO_TEST_REDIS_ADDR not set")
	}
	r, err := NewRedis(RedisConfig{Addr: addr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	ctx := context.Background()
	key := "ncgo-test-" + t.Name()
	incrKey := key + ":n"
	_ = r.Delete(ctx, key)
	_ = r.Delete(ctx, incrKey)
	t.Cleanup(func() {
		_ = r.Delete(ctx, key)
		_ = r.Delete(ctx, incrKey)
	})
	if _, err := r.Get(ctx, key); !errors.Is(err, ErrMiss) {
		t.Fatalf("miss = %v", err)
	}
	if err := r.Set(ctx, key, []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, key)
	if err != nil || string(got) != "v" {
		t.Fatalf("get = %q %v", got, err)
	}
	n, err := r.Increment(ctx, incrKey, 5)
	if err != nil || n != 5 {
		t.Fatalf("incr = %d %v", n, err)
	}
	// DeleteByPrefix removes only keys under the prefix; the control keys
	// with different prefixes survive.
	pfx := key + ":sub:"
	for _, k := range []string{pfx + "a", pfx + "b"} {
		if err := r.Set(ctx, k, []byte("v"), time.Minute); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Delete(ctx, k) })
	}
	if n, err := r.DeleteByPrefix(ctx, pfx); err != nil || n != 2 {
		t.Fatalf("deletebyprefix = %d %v", n, err)
	}
	if _, err := r.Get(ctx, pfx+"a"); !errors.Is(err, ErrMiss) {
		t.Fatalf("deleted key = %v", err)
	}
	if got, err := r.Get(ctx, incrKey); err != nil || string(got) != "5" {
		t.Fatalf("control key lost = %q %v", got, err)
	}
	if err := r.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
}
