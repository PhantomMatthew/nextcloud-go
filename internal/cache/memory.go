package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto/v2"
)

// MemoryConfig sizes the in-process L1 cache.
type MemoryConfig struct {
	MaxItems     int64
	MaxCostBytes int64
}

// memoryEntry pairs the value with its key: ristretto reports evictions and
// rejections as an Item carrying only the hashed key, so the original string
// key must travel with the value for the callbacks to untrack it.
type memoryEntry struct {
	key string
	val []byte
}

// Memory is a ristretto-backed Cache. Increment uses a separate mutex map.
// keys mirrors the keys actually stored (ristretto has no key iteration, so
// DeleteByPrefix needs its own index); it shares inc's mutex.
type Memory struct {
	c    *ristretto.Cache[string, memoryEntry]
	mu   sync.Mutex
	inc  map[string]int64
	keys map[string]struct{}
}

// NewMemory constructs an L1 cache. Zero MaxItems/MaxCostBytes become 1e5 / 64MiB.
func NewMemory(cfg MemoryConfig) (*Memory, error) {
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = 100_000
	}
	if cfg.MaxCostBytes <= 0 {
		cfg.MaxCostBytes = 64 << 20
	}
	m := &Memory{inc: make(map[string]int64), keys: make(map[string]struct{})}
	c, err := ristretto.NewCache(&ristretto.Config[string, memoryEntry]{
		NumCounters: cfg.MaxItems * 10,
		MaxCost:     cfg.MaxCostBytes,
		BufferItems: 64,
		// Both callbacks run on ristretto's background goroutine (OnEvict on
		// policy evictions and the periodic TTL cleanup, OnReject on
		// admission refusal). Never hold m.mu across a cache call such as
		// Set/Wait — the goroutine blocking on m.mu while the caller waits
		// for the set buffer to drain would deadlock.
		OnEvict: func(item *ristretto.Item[memoryEntry]) {
			m.untrack(item.Value.key)
		},
		OnReject: func(item *ristretto.Item[memoryEntry]) {
			m.untrack(item.Value.key)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("cache: memory: %w", err)
	}
	m.c = c
	return m, nil
}

func (m *Memory) untrack(key string) {
	m.mu.Lock()
	delete(m.keys, key)
	m.mu.Unlock()
}

// Close releases ristretto resources.
func (m *Memory) Close() {
	if m != nil && m.c != nil {
		m.c.Close()
	}
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	ent, ok := m.c.Get(key)
	if !ok {
		return nil, ErrMiss
	}
	out := make([]byte, len(ent.val))
	copy(out, ent.val)
	return out, nil
}

func (m *Memory) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	cost := int64(len(val))
	if cost == 0 {
		cost = 1
	}
	// Track before the ristretto call: OnEvict/OnReject can only fire once
	// the item enters the set buffer, so the callbacks below always find
	// the key present. Wait() then guarantees the admission decision (and
	// any untrack) landed before Set returns.
	m.mu.Lock()
	m.keys[key] = struct{}{}
	m.mu.Unlock()
	stored := memoryEntry{key: key, val: append([]byte(nil), val...)}
	var ok bool
	if ttl > 0 {
		ok = m.c.SetWithTTL(key, stored, cost, ttl)
	} else {
		ok = m.c.Set(key, stored, cost)
	}
	if !ok {
		// Buffer drop: no callback fires for items that never enter the
		// buffer, so untrack here.
		m.untrack(key)
		return fmt.Errorf("cache: memory set rejected for %q", key)
	}
	m.c.Wait()
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.c.Del(key)
	m.mu.Lock()
	delete(m.inc, key)
	delete(m.keys, key)
	m.mu.Unlock()
	return nil
}

func (m *Memory) DeleteByPrefix(_ context.Context, prefix string) (int64, error) {
	if prefix == "" {
		return 0, ErrEmptyPrefix
	}
	m.mu.Lock()
	matched := make(map[string]struct{})
	for k := range m.keys {
		if strings.HasPrefix(k, prefix) {
			matched[k] = struct{}{}
		}
	}
	for k := range m.inc {
		if strings.HasPrefix(k, prefix) {
			matched[k] = struct{}{}
		}
	}
	m.mu.Unlock()
	// Del outside the lock per the deadlock note in NewMemory. keys mirrors
	// the store, but a TTL-expired key may linger until the cleanup ticker
	// reports it — Del on it is a harmless no-op.
	for k := range matched {
		m.c.Del(k)
	}
	m.mu.Lock()
	for k := range matched {
		delete(m.keys, k)
		delete(m.inc, k)
	}
	m.mu.Unlock()
	return int64(len(matched)), nil
}

func (m *Memory) Increment(_ context.Context, key string, delta int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inc[key] += delta
	return m.inc[key], nil
}
