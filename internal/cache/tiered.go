package cache

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// Tiered is an L1 + optional L2 cache. Get fills L1 from L2 on miss.
// Set writes through both layers. Increment prefers L2 as the authority.
type Tiered struct {
	l1 Cache
	l2 Cache
}

// NewTiered wraps l1 and optional l2. l2 may be nil.
func NewTiered(l1, l2 Cache) *Tiered {
	return &Tiered{l1: l1, l2: l2}
}

func (t *Tiered) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := t.l1.Get(ctx, key)
	if err == nil {
		return val, nil
	}
	if !errors.Is(err, ErrMiss) {
		return nil, err
	}
	if t.l2 == nil {
		return nil, ErrMiss
	}
	val, err = t.l2.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	setBestEffort(t.l1, ctx, key, val, defaultFillTTL)
	return val, nil
}

func (t *Tiered) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if err := t.l1.Set(ctx, key, val, ttl); err != nil {
		return err
	}
	if t.l2 != nil {
		return t.l2.Set(ctx, key, val, ttl)
	}
	return nil
}

func (t *Tiered) Delete(ctx context.Context, key string) error {
	err1 := t.l1.Delete(ctx, key)
	if t.l2 == nil {
		return err1
	}
	err2 := t.l2.Delete(ctx, key)
	if err1 != nil {
		return err1
	}
	return err2
}

func (t *Tiered) Increment(ctx context.Context, key string, delta int64) (int64, error) {
	if t.l2 != nil {
		n, err := t.l2.Increment(ctx, key, delta)
		if err != nil {
			return 0, err
		}
		setBestEffort(t.l1, ctx, key, strconv.AppendInt(nil, n, 10), defaultFillTTL)
		return n, nil
	}
	return t.l1.Increment(ctx, key, delta)
}

func setBestEffort(c Cache, ctx context.Context, key string, val []byte, ttl time.Duration) {
	if err := c.Set(ctx, key, val, ttl); err != nil {
		return
	}
}
