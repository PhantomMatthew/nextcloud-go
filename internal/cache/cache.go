package cache

import (
	"context"
	"errors"
	"time"
)

// ErrMiss is returned when a key is not present in the cache.
var ErrMiss = errors.New("cache: miss")

// ErrEmptyPrefix is returned by DeleteByPrefix for an empty prefix, which
// would otherwise match the entire keyspace (a Redis MATCH "*" footgun).
var ErrEmptyPrefix = errors.New("cache: empty prefix")

// Cache is the unified byte cache used by the host and modules.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	// DeleteByPrefix removes every key starting with prefix and returns how
	// many were deleted. An empty prefix is rejected with ErrEmptyPrefix.
	DeleteByPrefix(ctx context.Context, prefix string) (int64, error)
	Increment(ctx context.Context, key string, delta int64) (int64, error)
}

const defaultFillTTL = 30 * time.Second
