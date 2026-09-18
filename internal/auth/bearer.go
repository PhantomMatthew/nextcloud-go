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
}

func (v *BearerVerifier) VerifyToken(ctx context.Context, token string) (*Principal, error) {
	token = strings.TrimSpace(token)
	if token == "" || v == nil || v.Store == nil {
		return nil, ErrInvalidCredentials
	}
	if uid, ok := v.cachedUID(ctx, token); ok {
		return v.principalForUID(ctx, uid, AuthMethodBearer)
	}
	t, err := v.Store.GetByHash(ctx, HashToken(token, v.Secret))
	if errors.Is(err, ErrTokenNotFound) {
		t, err = v.Store.GetByHash(ctx, hashTokenLegacy(token))
	}
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	p, err := v.principalForUID(ctx, t.UID, AuthMethodBearer)
	if err != nil {
		return nil, err
	}
	v.rememberUID(ctx, token, t.UID)
	return p, nil
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
