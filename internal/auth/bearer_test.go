package auth

import (
	"context"
	"errors"
	"testing"
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
