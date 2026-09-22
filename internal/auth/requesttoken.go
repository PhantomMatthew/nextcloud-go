package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// RequestTokenHeader is both the HTTP header and the form field browsers use
// to echo the requesttoken back on unsafe requests.
const RequestTokenHeader = "requesttoken"

// Token derivation domains: separate prefixes keep session tokens and
// anonymous login tokens in distinct HMAC domains, so one can never be
// replayed as the other.
const (
	sessionScope = "ncgo-requesttoken:"
	loginScope   = "ncgo-login:"
)

// RequestToken derives per-session CSRF tokens statelessly from the instance
// secret: token = base64url(HMAC-SHA256(secret, domain || id)). Derivation is
// deterministic, so verification needs no token storage (ADR-0064).
type RequestToken struct {
	secret []byte
}

func NewRequestToken(secret string) *RequestToken {
	return &RequestToken{secret: []byte(secret)}
}

func (t *RequestToken) mac(domain, id string) []byte {
	m := hmac.New(sha256.New, t.secret)
	_, _ = m.Write([]byte(domain)) // hash.Hash.Write never errors
	_, _ = m.Write([]byte(id))     // hash.Hash.Write never errors
	return m.Sum(nil)
}

// Derive returns the requesttoken bound to session sid.
func (t *RequestToken) Derive(sid string) string {
	return base64.RawURLEncoding.EncodeToString(t.mac(sessionScope, sid))
}

// Verify reports whether token is the requesttoken for session sid, in
// constant time on the decoded MAC.
func (t *RequestToken) Verify(sid, token string) bool {
	return t.verify(sessionScope, sid, token)
}

// DeriveLoginToken returns the anonymous login-page token bound to nonce.
func (t *RequestToken) DeriveLoginToken(nonce string) string {
	return base64.RawURLEncoding.EncodeToString(t.mac(loginScope, nonce))
}

// VerifyLoginToken reports whether token is the login token for nonce.
func (t *RequestToken) VerifyLoginToken(nonce, token string) bool {
	return t.verify(loginScope, nonce, token)
}

func (t *RequestToken) verify(domain, id, token string) bool {
	if t == nil || id == "" || token == "" {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	return hmac.Equal(t.mac(domain, id), got)
}

// NewLoginNonce returns 32 random bytes hex-encoded, arming the double-submit
// cookie of the anonymous login page.
func NewLoginNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: login nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
