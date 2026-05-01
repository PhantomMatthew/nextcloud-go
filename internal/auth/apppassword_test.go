package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGenerateToken_LengthAndAlphabet(t *testing.T) {
	for i := 0; i < 50; i++ {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if len(tok) != TokenLength {
			t.Fatalf("len=%d want=%d", len(tok), TokenLength)
		}
		for _, c := range tok {
			if !strings.ContainsRune(TokenAlphabet, c) {
				t.Fatalf("char %q not in alphabet", c)
			}
		}
	}
}

func TestGenerateToken_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token at i=%d", i)
		}
		seen[tok] = struct{}{}
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	h1 := HashToken("abc", "secret")
	h2 := HashToken("abc", "secret")
	if h1 != h2 {
		t.Fatalf("hash mismatch: %s vs %s", h1, h2)
	}
	if HashToken("abc", "secret") == HashToken("abc", "other") {
		t.Fatalf("hash should differ with secret")
	}
	if len(h1) != 128 {
		t.Fatalf("sha512 hex len=%d want=128", len(h1))
	}
}

func TestMemoryStore_CRUD(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	tok := &Token{ID: "id1", Hash: "h1", UID: "alice", LoginName: "alice", Type: TokenTypePermanent}
	if err := s.Insert(ctx, tok); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.GetByHash(ctx, "h1")
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got.UID != "alice" {
		t.Fatalf("UID=%q want alice", got.UID)
	}
	got.UID = "mutated"
	again, _ := s.GetByHash(ctx, "h1")
	if again.UID != "alice" {
		t.Fatalf("store mutated by caller: %q", again.UID)
	}
	if err := s.DeleteByHash(ctx, "h1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetByHash(ctx, "h1"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("want ErrTokenNotFound, got %v", err)
	}
	if err := s.DeleteByHash(ctx, "h1"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("delete missing: want ErrTokenNotFound, got %v", err)
	}
	if err := s.Insert(ctx, &Token{}); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("insert empty: want ErrTokenInvalid, got %v", err)
	}
}

func TestIssueAndVerifyAppPassword(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	raw, _, err := IssueAppPassword(ctx, store, "pepper", "alice", "alice", "iPhone", TokenTypePermanent)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(raw) != TokenLength {
		t.Fatalf("raw len=%d", len(raw))
	}
	v := NewAppPasswordVerifier(store, "pepper")
	p, err := v.Verify(ctx, "alice", raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.UID != "alice" || p.AuthMethod != AuthMethodAppPassword {
		t.Fatalf("principal=%+v", p)
	}
	if _, err := v.Verify(ctx, "bob", raw); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong user: want ErrInvalidCredentials, got %v", err)
	}
	if _, err := v.Verify(ctx, "alice", "short"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("short pw: want ErrInvalidCredentials, got %v", err)
	}
	if _, err := v.Verify(ctx, "alice", ""); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("empty pw: want ErrNoCredentials, got %v", err)
	}
	if _, err := v.Verify(ctx, "alice", strings.Repeat("z", TokenLength)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown token: want ErrInvalidCredentials, got %v", err)
	}
}

func TestRevokeAppPassword(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	raw, _, err := IssueAppPassword(ctx, store, "pep", "u", "u", "n", TokenTypePermanent)
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeAppPassword(ctx, store, "pep", raw); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	v := NewAppPasswordVerifier(store, "pep")
	if _, err := v.Verify(ctx, "u", raw); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("post-revoke verify: want ErrInvalidCredentials, got %v", err)
	}
}

func TestChainVerifier_Order(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	raw, _, _ := IssueAppPassword(ctx, store, "pep", "alice", "alice", "n", TokenTypePermanent)

	apv := NewAppPasswordVerifier(store, "pep")
	sv := NewStaticVerifier("admin", "admin", "admin")
	chain := NewChainVerifier(apv, sv)

	p, err := chain.Verify(ctx, "alice", raw)
	if err != nil {
		t.Fatalf("app password through chain: %v", err)
	}
	if p.AuthMethod != AuthMethodAppPassword {
		t.Fatalf("AuthMethod=%q want app_password", p.AuthMethod)
	}

	p, err = chain.Verify(ctx, "admin", "admin")
	if err != nil {
		t.Fatalf("static through chain: %v", err)
	}
	if p.AuthMethod != AuthMethodBasic {
		t.Fatalf("AuthMethod=%q want basic", p.AuthMethod)
	}

	if _, err := chain.Verify(ctx, "ghost", "wrong-but-long-enough-password!!!"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("bad creds: want ErrInvalidCredentials, got %v", err)
	}
}

func TestChainVerifier_Empty(t *testing.T) {
	if _, err := NewChainVerifier().Verify(context.Background(), "u", "p"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("empty chain: want ErrInvalidCredentials, got %v", err)
	}
}
