package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	TokenAlphabet  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	TokenLength    = 72
	TokenMinLength = 22

	TokenTypeTemporary = 0
	TokenTypePermanent = 1
	TokenTypeWipe      = 2
	TokenTypeOneTime   = 3
)

var (
	ErrTokenNotFound = errors.New("auth: token not found")
	ErrTokenInvalid  = errors.New("auth: token invalid")
)

type Token struct {
	ID        string
	Hash      string
	UID       string
	LoginName string
	Name      string
	Type      int
	CreatedAt time.Time
}

type Store interface {
	Insert(ctx context.Context, t *Token) error
	GetByHash(ctx context.Context, hash string) (*Token, error)
	DeleteByHash(ctx context.Context, hash string) error
}

type MemoryStore struct {
	mu     sync.RWMutex
	byHash map[string]*Token
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byHash: make(map[string]*Token)}
}

func (s *MemoryStore) Insert(_ context.Context, t *Token) error {
	if t == nil || t.Hash == "" {
		return ErrTokenInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	s.byHash[t.Hash] = &cp
	return nil
}

func (s *MemoryStore) GetByHash(_ context.Context, hash string) (*Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byHash[hash]
	if !ok {
		return nil, ErrTokenNotFound
	}
	cp := *t
	return &cp, nil
}

func (s *MemoryStore) DeleteByHash(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byHash[hash]; !ok {
		return ErrTokenNotFound
	}
	delete(s.byHash, hash)
	return nil
}

func GenerateToken() (string, error) {
	out := make([]byte, TokenLength)
	alpha := []byte(TokenAlphabet)
	alphaLen := len(alpha)
	limit := 256 - (256 % alphaLen)
	buf := make([]byte, TokenLength*2)
	filled := 0
	for filled < TokenLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("auth: read random: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out[filled] = alpha[int(b)%alphaLen]
			filled++
			if filled == TokenLength {
				break
			}
		}
	}
	return string(out), nil
}

func HashToken(token, secret string) string {
	sum := sha512.Sum512([]byte(token + secret))
	return hex.EncodeToString(sum[:])
}

func hashTokenLegacy(token string) string {
	sum := sha512.Sum512([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AppTokenKeyUnlocker opens an app-password token's wrap of the user's
// X25519 private key (ADR-0102 app-token key wraps). *encrypt.SQLResolver
// satisfies it structurally; auth must not import encrypt, so the seam is
// declared here. A nil return means the token carries no wrap
// (pre-enrollment or imported token) and the request attaches no key.
type AppTokenKeyUnlocker interface {
	UnlockForToken(ctx context.Context, tokenHash, tokenRaw string) ([]byte, error)
}

type AppPasswordVerifier struct {
	Store  Store
	Secret string
	// Keys, when non-nil, opens the verified token's key wrap and attaches
	// the unlocked private key to the Principal (ADR-0102). Nil-ok: no key
	// attach, phase 4-a behavior.
	Keys AppTokenKeyUnlocker
}

func NewAppPasswordVerifier(store Store, secret string) *AppPasswordVerifier {
	return &AppPasswordVerifier{Store: store, Secret: secret}
}

func (v *AppPasswordVerifier) Verify(ctx context.Context, user, password string) (*Principal, error) {
	if password == "" {
		return nil, ErrNoCredentials
	}
	if len(password) < TokenMinLength {
		return nil, ErrInvalidCredentials
	}

	t, err := v.Store.GetByHash(ctx, HashToken(password, v.Secret))
	if errors.Is(err, ErrTokenNotFound) {
		t, err = v.Store.GetByHash(ctx, hashTokenLegacy(password))
	}
	if err != nil {
		return nil, ErrInvalidCredentials
	}

	if user != "" {
		uOK := subtle.ConstantTimeCompare([]byte(user), []byte(t.UID)) == 1
		lOK := subtle.ConstantTimeCompare([]byte(user), []byte(t.LoginName)) == 1
		if !uOK && !lOK {
			return nil, ErrInvalidCredentials
		}
	}

	p := &Principal{
		UID:        t.UID,
		Enabled:    true,
		AuthMethod: AuthMethodAppPassword,
	}
	if v.Keys != nil {
		// t.Hash is the row's stored hash — it matches whichever of the
		// primary/legacy lookups above found the row, so one wrap lookup
		// suffices. Fail closed: a corrupt wrap must not silently degrade to
		// per-file 403s (mirroring the 4-a session-copy semantics).
		priv, err := v.Keys.UnlockForToken(ctx, t.Hash, password)
		if err != nil {
			return nil, err
		}
		p.UnlockedKey = priv
	}
	return p, nil
}

func IssueAppPassword(ctx context.Context, store Store, secret, uid, loginName, name string, tokenType int) (string, *Token, error) {
	raw, err := GenerateToken()
	if err != nil {
		return "", nil, err
	}
	t := &Token{
		ID:        raw[:16],
		Hash:      HashToken(raw, secret),
		UID:       uid,
		LoginName: loginName,
		Name:      name,
		Type:      tokenType,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Insert(ctx, t); err != nil {
		return "", nil, err
	}
	return raw, t, nil
}

func RevokeAppPassword(ctx context.Context, store Store, secret, raw string) error {
	if err := store.DeleteByHash(ctx, HashToken(raw, secret)); err == nil {
		return nil
	}
	return store.DeleteByHash(ctx, hashTokenLegacy(raw))
}
