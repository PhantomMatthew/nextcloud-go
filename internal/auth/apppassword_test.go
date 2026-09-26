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

// stubTokenKeys is an AppTokenKeyUnlocker fake recording its calls and
// answering per stored hash.
type stubTokenKeys struct {
	calls  [][2]string
	byHash map[string][]byte
	err    error
}

func (s *stubTokenKeys) UnlockForToken(_ context.Context, tokenHash, tokenRaw string) ([]byte, error) {
	s.calls = append(s.calls, [2]string{tokenHash, tokenRaw})
	if s.err != nil {
		return nil, s.err
	}
	return s.byHash[tokenHash], nil
}

// TestAppPasswordVerifierAttachesTokenKey pins the ADR-0102 unlock seam: a
// verified token's wrap opens onto the principal (keyed by the row's STORED
// hash), no wrap attaches nothing, and an unlock error fails closed.
func TestAppPasswordVerifierAttachesTokenKey(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	secret := "pepper"
	raw, _, err := IssueAppPassword(ctx, store, secret, "alice", "alice", "iPhone", TokenTypePermanent)
	if err != nil {
		t.Fatal(err)
	}
	priv := []byte("0123456789abcdef0123456789abcdef")

	t.Run("wrap attaches the key", func(t *testing.T) {
		keys := &stubTokenKeys{byHash: map[string][]byte{HashToken(raw, secret): priv}}
		v := NewAppPasswordVerifier(store, secret)
		v.Keys = keys
		p, err := v.Verify(ctx, "alice", raw)
		if err != nil {
			t.Fatal(err)
		}
		if string(p.UnlockedKey) != string(priv) {
			t.Errorf("UnlockedKey = %q, want the wrapped key", p.UnlockedKey)
		}
		if len(keys.calls) != 1 || keys.calls[0] != [2]string{HashToken(raw, secret), raw} {
			t.Errorf("unlock calls = %v, want the stored primary hash + raw token", keys.calls)
		}
	})

	t.Run("no wrap attaches nothing", func(t *testing.T) {
		v := NewAppPasswordVerifier(store, secret)
		v.Keys = &stubTokenKeys{}
		p, err := v.Verify(ctx, "alice", raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.UnlockedKey != nil {
			t.Errorf("UnlockedKey without a wrap = %q, want nil", p.UnlockedKey)
		}
	})

	t.Run("unlock error fails closed", func(t *testing.T) {
		boom := errors.New("token key wrap authentication failed")
		v := NewAppPasswordVerifier(store, secret)
		v.Keys = &stubTokenKeys{err: boom}
		if _, err := v.Verify(ctx, "alice", raw); !errors.Is(err, boom) {
			t.Errorf("verify err = %v, want the unlock error (fail-closed)", err)
		}
	})

	t.Run("legacy row unlocks by its stored hash", func(t *testing.T) {
		legacyRaw := "legacy-token-0123456789abcdef0123456789abcdef"
		legacyHash := hashTokenLegacy(legacyRaw)
		if err := store.Insert(ctx, &Token{
			ID: "id-legacy", Hash: legacyHash, UID: "alice", LoginName: "alice", Type: TokenTypePermanent,
		}); err != nil {
			t.Fatal(err)
		}
		keys := &stubTokenKeys{byHash: map[string][]byte{legacyHash: priv}}
		v := NewAppPasswordVerifier(store, secret)
		v.Keys = keys
		p, err := v.Verify(ctx, "alice", legacyRaw)
		if err != nil {
			t.Fatal(err)
		}
		if string(p.UnlockedKey) != string(priv) {
			t.Errorf("legacy UnlockedKey = %q, want the wrapped key", p.UnlockedKey)
		}
		// One call, with the row's stored (legacy) hash — the legacy fallback
		// in the token lookup already settled which hash the row carries.
		if len(keys.calls) != 1 || keys.calls[0] != [2]string{legacyHash, legacyRaw} {
			t.Errorf("unlock calls = %v, want the stored legacy hash + raw token", keys.calls)
		}
	})

	t.Run("nil Keys attaches nothing", func(t *testing.T) {
		v := NewAppPasswordVerifier(store, secret)
		p, err := v.Verify(ctx, "alice", raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.UnlockedKey != nil {
			t.Errorf("UnlockedKey without Keys = %q, want nil", p.UnlockedKey)
		}
	})
}
