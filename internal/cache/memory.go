package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto/v2"
)

// MemoryConfig sizes the in-process L1 cache.
type MemoryConfig struct {
	MaxItems     int64
	MaxCostBytes int64
}

// Memory is a ristretto-backed Cache. Increment uses a separate mutex map.
type Memory struct {
	c   *ristretto.Cache[string, []byte]
	mu  sync.Mutex
	inc map[string]int64
}

// NewMemory constructs an L1 cache. Zero MaxItems/MaxCostBytes become 1e5 / 64MiB.
func NewMemory(cfg MemoryConfig) (*Memory, error) {
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = 100_000
	}
	if cfg.MaxCostBytes <= 0 {
		cfg.MaxCostBytes = 64 << 20
	}
	c, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: cfg.MaxItems * 10,
		MaxCost:     cfg.MaxCostBytes,
		BufferItems: 64,
	})
	if err != nil {
		return nil, fmt.Errorf("cache: memory: %w", err)
	}
	return &Memory{c: c, inc: make(map[string]int64)}, nil
}

// Close releases ristretto resources.
func (m *Memory) Close() {
	if m != nil && m.c != nil {
		m.c.Close()
	}
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	val, ok := m.c.Get(key)
	if !ok {
		return nil, ErrMiss
	}
	out := make([]byte, len(val))
	copy(out, val)
	return out, nil
}

func (m *Memory) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	cost := int64(len(val))
	if cost == 0 {
		cost = 1
	}
	stored := append([]byte(nil), val...)
	var ok bool
	if ttl > 0 {
		ok = m.c.SetWithTTL(key, stored, cost, ttl)
	} else {
		ok = m.c.Set(key, stored, cost)
	}
	if !ok {
		return fmt.Errorf("cache: memory set rejected for %q", key)
	}
	m.c.Wait()
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.c.Del(key)
	m.mu.Lock()
	delete(m.inc, key)
	m.mu.Unlock()
	return nil
}

func (m *Memory) Increment(_ context.Context, key string, delta int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inc[key] += delta
	return m.inc[key], nil
}
