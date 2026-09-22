package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig selects a Redis database.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// Redis is an L2 Cache backed by go-redis.
type Redis struct {
	c *redis.Client
}

// NewRedis pings a Redis client.
func NewRedis(cfg RedisConfig) (*Redis, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("cache: redis addr required")
	}
	c := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	if err := c.Ping(context.Background()).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("cache: redis ping: %w", err)
	}
	return &Redis{c: c}, nil
}

// Close closes the Redis client.
func (r *Redis) Close() error {
	if r == nil || r.c == nil {
		return nil
	}
	return r.c.Close()
}

func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := r.c.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, fmt.Errorf("cache: redis get: %w", err)
	}
	return val, nil
}

func (r *Redis) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if err := r.c.Set(ctx, key, val, ttl).Err(); err != nil {
		return fmt.Errorf("cache: redis set: %w", err)
	}
	return nil
}

func (r *Redis) Delete(ctx context.Context, key string) error {
	if err := r.c.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("cache: redis del: %w", err)
	}
	return nil
}

func (r *Redis) DeleteByPrefix(ctx context.Context, prefix string) (int64, error) {
	if prefix == "" {
		return 0, ErrEmptyPrefix
	}
	// SCAN cursor loop with the prefix glob-escaped so it matches literally.
	match := redisMatchPattern(prefix) + "*"
	var cursor uint64
	var total int64
	for {
		keys, next, err := r.c.Scan(ctx, cursor, match, 200).Result()
		if err != nil {
			return total, fmt.Errorf("cache: redis scan: %w", err)
		}
		if len(keys) > 0 {
			// UNLINK (Redis >= 4) defers the actual free to a background
			// thread, unlike blocking DEL — a large keyspace never stalls
			// the server.
			n, err := r.c.Unlink(ctx, keys...).Result()
			if err != nil {
				return total, fmt.Errorf("cache: redis unlink: %w", err)
			}
			total += n
		}
		if next == 0 {
			return total, nil
		}
		cursor = next
	}
}

// redisMatchPattern escapes the Redis glob metacharacters in prefix so a
// SCAN MATCH treats it as a literal string.
func redisMatchPattern(prefix string) string {
	var sb strings.Builder
	sb.Grow(len(prefix))
	for _, r := range prefix {
		switch r {
		case '\\', '*', '?', '[', ']':
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func (r *Redis) Increment(ctx context.Context, key string, delta int64) (int64, error) {
	n, err := r.c.IncrBy(ctx, key, delta).Result()
	if err != nil {
		return 0, fmt.Errorf("cache: redis incr: %w", err)
	}
	return n, nil
}
