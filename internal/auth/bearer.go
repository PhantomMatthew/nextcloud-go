package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
)

const tokenCacheTTL = 60 * time.Second

// BearerVerifier authenticates Authorization: Bearer app-password tokens.
type BearerVerifier struct {
	Store  Store
	Users  UserSource
	Secret string
	Cache  cache.Cache
	// Keys, when non-nil, opens the verified token's key wrap and attaches
	// the unlocked private key to the Principal (ADR-0102 — the same seam
	// the app-password verifier uses). Nil-ok: no key attach.
	Keys AppTokenKeyUnlocker
}

func (v *BearerVerifier) VerifyToken(ctx context.Context, token string) (*Principal, error) {
	token = strings.TrimSpace(token)
	if token == "" || v == nil || v.Store == nil {
		return nil, ErrInvalidCredentials
	}
	var p *Principal
	if uid, ok := v.cachedUID(ctx, token); ok {
		var err error
		p, err = v.principalForUID(ctx, uid, AuthMethodBearer)
		if err != nil {
			return nil, err
		}
	} else {
		t, err := v.Store.GetByHash(ctx, HashToken(token, v.Secret))
		if errors.Is(err, ErrTokenNotFound) {
			t, err = v.Store.GetByHash(ctx, hashTokenLegacy(token))
		}
		if err != nil {
			return nil, ErrInvalidCredentials
		}
		p, err = v.principalForUID(ctx, t.UID, AuthMethodBearer)
		if err != nil {
			return nil, err
		}
		v.rememberUID(ctx, token, t.UID)
	}
	if err := v.attachTokenKey(ctx, token, p); err != nil {
		return nil, err
	}
	return p, nil
}

// attachTokenKey opens the token's app_token_keys wrap (ADR-0102) and
// attaches the unlocked private key to p — on the cache-hit path too: the
// uid cache deliberately caches no key material, so every request re-derives
// the token KEK (a cheap HMAC + HKDF) and re-opens the wrap. The wrap row is
// keyed by the row's stored token hash, so the primary hash is tried first
// and the legacy hash only when no wrap exists under it (legacy-era tokens).
// A nil result attaches nothing (pre-enrollment or imported token); an open
// failure is fail-closed, mirroring the app-password verifier.
func (v *BearerVerifier) attachTokenKey(ctx context.Context, token string, p *Principal) error {
	if v.Keys == nil || p == nil {
		return nil
	}
	priv, err := v.Keys.UnlockForToken(ctx, HashToken(token, v.Secret), token)
	if err != nil {
		return err
	}
	if priv == nil {
		priv, err = v.Keys.UnlockForToken(ctx, hashTokenLegacy(token), token)
		if err != nil {
			return err
		}
	}
	p.UnlockedKey = priv
	return nil
}

func (v *BearerVerifier) cachedUID(ctx context.Context, token string) (string, bool) {
	if v.Cache == nil {
		return "", false
	}
	b, err := v.Cache.Get(ctx, tokenCacheKey(token))
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

func (v *BearerVerifier) rememberUID(ctx context.Context, token, uid string) {
	if v.Cache == nil || uid == "" {
		return
	}
	if err := v.Cache.Set(ctx, tokenCacheKey(token), []byte(uid), tokenCacheTTL); err != nil {
		return
	}
}

func (v *BearerVerifier) principalForUID(ctx context.Context, uid, method string) (*Principal, error) {
	if v.Users == nil {
		return &Principal{UID: uid, Enabled: true, AuthMethod: method}, nil
	}
	u, err := v.Users.GetByUID(ctx, uid)
	if err != nil || !u.Enabled {
		return nil, ErrInvalidCredentials
	}
	return &Principal{UID: u.UID, DisplayName: u.DisplayName, Enabled: true, AuthMethod: method}, nil
}

func tokenCacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "auth:apppw:" + hex.EncodeToString(sum[:])
}
