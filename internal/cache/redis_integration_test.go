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
	_ = r.Delete(ctx, key)
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
	n, err := r.Increment(ctx, key+":n", 5)
	if err != nil || n != 5 {
		t.Fatalf("incr = %d %v", n, err)
	}
	if err := r.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
}
