package auth

import (
	"context"
	"strconv"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
)

// Throttler records auth attempts and returns a delay for excessive retries.
type Throttler interface {
	Observe(ctx context.Context, ip, action string, now time.Time) (delay time.Duration, err error)
}

// CacheThrottler counts attempts in cache buckets of Window.
type CacheThrottler struct {
	cache  cache.Cache
	limit  int64
	window time.Duration
}

func NewCacheThrottler(c cache.Cache, limit int64, window time.Duration) *CacheThrottler {
	if limit <= 0 {
		limit = 8
	}
	if window <= 0 {
		window = 30 * time.Second
	}
	return &CacheThrottler{cache: c, limit: limit, window: window}
}

func (t *CacheThrottler) Observe(ctx context.Context, ip, action string, now time.Time) (time.Duration, error) {
	if t == nil || t.cache == nil {
		return 0, nil
	}
	if ip == "" {
		ip = "unknown"
	}
	if action == "" {
		action = "login"
	}
	secs := int64(t.window.Seconds())
	if secs <= 0 {
		secs = 30
	}
	bucket := now.Unix() / secs
	key := "bf:" + ip + ":" + action + ":" + strconv.FormatInt(bucket, 10)
	n, err := t.cache.Increment(ctx, key, 1)
	if err != nil {
		return 0, err
	}
	if n <= t.limit {
		return 0, nil
	}
	return time.Duration(n-t.limit) * 100 * time.Millisecond, nil
}
