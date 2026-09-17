package cache

import (
	"context"
	"errors"
	"time"
)

// ErrMiss is returned when a key is not present in the cache.
var ErrMiss = errors.New("cache: miss")

// Cache is the unified byte cache used by the host and modules.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Increment(ctx context.Context, key string, delta int64) (int64, error)
}

const defaultFillTTL = 30 * time.Second
