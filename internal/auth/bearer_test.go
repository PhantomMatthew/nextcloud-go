package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
)

func TestBearerVerifierAppPassword(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	secret := "s"
	raw, _, err := IssueAppPassword(ctx, store, secret, "alice", "alice", "desktop", TokenTypePermanent)
	if err != nil {
		t.Fatal(err)
	}
	v := &BearerVerifier{Store: store, Secret: secret}
	p, err := v.VerifyToken(ctx, raw)
	if err != nil || p.UID != "alice" || p.AuthMethod != AuthMethodBearer {
		t.Fatalf("got %+v %v", p, err)
	}
	if _, err := v.VerifyToken(ctx, "no-such-token-value-minlen22"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("bad token = %v", err)
	}
}

// TestBearerVerifierAttachesTokenKey pins the ADR-0102 unlock on BOTH bearer
// paths: cache-miss (token row lookup) and cache-hit (uid cache). The uid
// cache format is untouched — key material is re-opened per request.
func TestBearerVerifierAttachesTokenKey(t *testing.T) {
	ctx := context.Background()
	secret := "s"
	priv := []byte("0123456789abcdef0123456789abcdef")

	t.Run("cache miss", func(t *testing.T) {
		store := NewMemoryStore()
		raw, _, err := IssueAppPassword(ctx, store, secret, "alice", "alice", "desktop", TokenTypePermanent)
		if err != nil {
			t.Fatal(err)
		}
		keys := &stubTokenKeys{byHash: map[string][]byte{HashToken(raw, secret): priv}}
		v := &BearerVerifier{Store: store, Secret: secret, Keys: keys}
		p, err := v.VerifyToken(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if string(p.UnlockedKey) != string(priv) {
			t.Errorf("UnlockedKey = %q, want the wrapped key", p.UnlockedKey)
		}
		if len(keys.calls) != 1 || keys.calls[0] != [2]string{HashToken(raw, secret), raw} {
			t.Errorf("unlock calls = %v, want the primary hash + raw token", keys.calls)
		}
	})

	t.Run("cache hit", func(t *testing.T) {
		// An EMPTY store: the uid comes from the cache — the store is never
		// consulted, but the wrap still opens per request.
		store := NewMemoryStore()
		raw := "cached-token-0123456789abcdef0123456789abcdef"
		c, err := cache.NewMemory(cache.MemoryConfig{MaxItems: 16, MaxCostBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Set(ctx, tokenCacheKey(raw), []byte("alice"), time.Minute); err != nil {
			t.Fatal(err)
		}
		keys := &stubTokenKeys{byHash: map[string][]byte{HashToken(raw, secret): priv}}
		v := &BearerVerifier{Store: store, Secret: secret, Cache: c, Keys: keys}
		p, err := v.VerifyToken(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.UID != "alice" || p.AuthMethod != AuthMethodBearer {
			t.Errorf("principal = %+v", p)
		}
		if string(p.UnlockedKey) != string(priv) {
			t.Errorf("cache-hit UnlockedKey = %q, want the wrapped key", p.UnlockedKey)
		}
	})

	t.Run("legacy fallback second call", func(t *testing.T) {
		store := NewMemoryStore()
		raw := "legacy-token-0123456789abcdef0123456789abcdef"
		legacyHash := hashTokenLegacy(raw)
		if err := store.Insert(ctx, &Token{
			ID: "id-legacy", Hash: legacyHash, UID: "alice", LoginName: "alice", Type: TokenTypePermanent,
		}); err != nil {
			t.Fatal(err)
		}
		keys := &stubTokenKeys{byHash: map[string][]byte{legacyHash: priv}}
		v := &BearerVerifier{Store: store, Secret: secret, Keys: keys}
		p, err := v.VerifyToken(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if string(p.UnlockedKey) != string(priv) {
			t.Errorf("legacy UnlockedKey = %q, want the wrapped key", p.UnlockedKey)
		}
		// Primary misses (nil), legacy hits: exactly two calls, in order.
		want := [][2]string{{HashToken(raw, secret), raw}, {legacyHash, raw}}
		if len(keys.calls) != 2 || keys.calls[0] != want[0] || keys.calls[1] != want[1] {
			t.Errorf("unlock calls = %v, want primary-then-legacy %v", keys.calls, want)
		}
	})

	t.Run("unlock error fails closed", func(t *testing.T) {
		store := NewMemoryStore()
		raw, _, err := IssueAppPassword(ctx, store, secret, "alice", "alice", "desktop", TokenTypePermanent)
		if err != nil {
			t.Fatal(err)
		}
		boom := errors.New("token key wrap authentication failed")
		v := &BearerVerifier{Store: store, Secret: secret, Keys: &stubTokenKeys{err: boom}}
		if _, err := v.VerifyToken(ctx, raw); !errors.Is(err, boom) {
			t.Errorf("verify err = %v, want the unlock error (fail-closed)", err)
		}
	})

	t.Run("no wrap attaches nothing", func(t *testing.T) {
		store := NewMemoryStore()
		raw, _, err := IssueAppPassword(ctx, store, secret, "alice", "alice", "desktop", TokenTypePermanent)
		if err != nil {
			t.Fatal(err)
		}
		v := &BearerVerifier{Store: store, Secret: secret, Keys: &stubTokenKeys{}}
		p, err := v.VerifyToken(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.UnlockedKey != nil {
			t.Errorf("UnlockedKey without a wrap = %q, want nil", p.UnlockedKey)
		}
	})
}
