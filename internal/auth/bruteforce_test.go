package auth

import (
	"context"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
)

func TestCacheThrottlerNinthDelay(t *testing.T) {
	ctx := context.Background()
	mem, err := cache.NewMemory(cache.MemoryConfig{MaxItems: 100, MaxCostBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mem.Close)
	th := NewCacheThrottler(mem, 8, 30*time.Second)
	now := time.Unix(1_700_000_000, 0).UTC()
	var last time.Duration
	for i := 0; i < 9; i++ {
		d, err := th.Observe(ctx, "127.0.0.1", "webdav", now)
		if err != nil {
			t.Fatal(err)
		}
		last = d
	}
	if last <= 0 {
		t.Fatalf("9th delay = %s, want > 0", last)
	}
}
