package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRequestTokenDeriveStable(t *testing.T) {
	t.Parallel()
	rt := NewRequestToken("instance-secret")
	a := rt.Derive("session-abc")
	b := rt.Derive("session-abc")
	if a == "" || a != b {
		t.Fatalf("derivation not deterministic: %q vs %q", a, b)
	}
	if _, err := base64.RawURLEncoding.DecodeString(a); err != nil {
		t.Fatalf("token must be base64url: %v", err)
	}
	if strings.ContainsAny(a, "+/=") {
		t.Fatalf("token must stay in the base64url alphabet: %q", a)
	}
	if !rt.Verify("session-abc", a) {
		t.Fatal("own token must verify")
	}
}

func TestRequestTokenRejectsTampering(t *testing.T) {
	t.Parallel()
	rt := NewRequestToken("instance-secret")
	good := rt.Derive("session-abc")
	cases := map[string]string{
		"other session":    rt.Derive("session-xyz"),
		"other secret":     NewRequestToken("other-secret").Derive("session-abc"),
		"login domain":     rt.DeriveLoginToken("session-abc"),
		"flipped char":     flipFirst(good),
		"truncated":        good[:len(good)-2],
		"empty":            "",
		"not base64":       "!!!",
		"wrong mac length": base64.RawURLEncoding.EncodeToString([]byte("short")),
	}
	for name, tok := range cases {
		if rt.Verify("session-abc", tok) {
			t.Errorf("%s: token must not verify", name)
		}
	}
}

func flipFirst(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func TestRequestTokenNilAndEmptyInputs(t *testing.T) {
	t.Parallel()
	var rt *RequestToken
	if rt.Verify("sid", "tok") {
		t.Fatal("nil RequestToken must not verify")
	}
	live := NewRequestToken("s")
	if live.Verify("", live.Derive("")) {
		t.Fatal("empty session id must not verify")
	}
	if live.Verify("sid", "") {
		t.Fatal("empty token must not verify")
	}
}

func TestLoginTokenDomainSeparation(t *testing.T) {
	t.Parallel()
	rt := NewRequestToken("instance-secret")
	nonce, err := NewLoginNonce()
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 64 {
		t.Fatalf("nonce = %d hex chars, want 64", len(nonce))
	}
	loginTok := rt.DeriveLoginToken(nonce)
	if !rt.VerifyLoginToken(nonce, loginTok) {
		t.Fatal("login token must verify against its nonce")
	}
	if rt.VerifyLoginToken("other-nonce", loginTok) {
		t.Fatal("login token must be bound to its nonce")
	}
	if rt.Verify(nonce, loginTok) {
		t.Fatal("login token must not verify as a session token")
	}
	sessTok := rt.Derive(nonce)
	if rt.VerifyLoginToken(nonce, sessTok) {
		t.Fatal("session token must not verify as a login token")
	}
}

func TestNewLoginNonceUnique(t *testing.T) {
	t.Parallel()
	a, err := NewLoginNonce()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewLoginNonce()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("nonces must be random")
	}
}
