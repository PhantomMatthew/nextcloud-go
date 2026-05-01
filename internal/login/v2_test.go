package login

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestServiceInitProducesUniqueTokens(t *testing.T) {
	svc := NewService(NewMemoryStore())
	ctx := context.Background()

	a, err := svc.Init(ctx, "Test Client/1.0")
	if err != nil {
		t.Fatalf("init a: %v", err)
	}
	b, err := svc.Init(ctx, "Test Client/1.0")
	if err != nil {
		t.Fatalf("init b: %v", err)
	}

	if a.PollToken == b.PollToken {
		t.Fatal("poll tokens must differ")
	}
	if a.LoginToken == b.LoginToken {
		t.Fatal("login tokens must differ")
	}
	if len(a.PollToken) != TokenLength {
		t.Fatalf("poll token length: got %d want %d", len(a.PollToken), TokenLength)
	}
	if len(a.LoginToken) != TokenLength {
		t.Fatalf("login token length: got %d want %d", len(a.LoginToken), TokenLength)
	}
	if a.State != StatePending {
		t.Fatalf("initial state: got %d want %d", a.State, StatePending)
	}
	if a.ClientName != "Test Client/1.0" {
		t.Fatalf("client name: got %q", a.ClientName)
	}
}

func TestPollPendingThenGrantedThenConsumed(t *testing.T) {
	svc := NewService(NewMemoryStore())
	ctx := context.Background()

	f, err := svc.Init(ctx, "ua")
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	if _, err := svc.Poll(ctx, f.PollToken); !errors.Is(err, ErrFlowPending) {
		t.Fatalf("expected pending, got %v", err)
	}

	g, err := svc.BeginGrant(ctx, f.LoginToken)
	if err != nil {
		t.Fatalf("begin grant: %v", err)
	}
	if g.StateToken == "" {
		t.Fatal("state token should be set")
	}
	if len(g.StateToken) != StateTokenLen*2 {
		t.Fatalf("state token hex length: got %d want %d", len(g.StateToken), StateTokenLen*2)
	}

	gr, err := svc.Grant(ctx, g.StateToken, "https://nc.example.test", "alice", "app-password-raw")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if gr.State != StateGranted {
		t.Fatalf("state after grant: got %d want %d", gr.State, StateGranted)
	}

	got, err := svc.Poll(ctx, f.PollToken)
	if err != nil {
		t.Fatalf("poll after grant: %v", err)
	}
	if got.Server != "https://nc.example.test" || got.LoginName != "alice" || got.AppPassword != "app-password-raw" {
		t.Fatalf("granted payload mismatch: %+v", got)
	}

	if _, err := svc.Poll(ctx, f.PollToken); !errors.Is(err, ErrFlowConsumed) {
		t.Fatalf("expected consumed on second poll, got %v", err)
	}
}

func TestPollExpired(t *testing.T) {
	store := NewMemoryStore()
	svc := NewService(store).WithTTL(50 * time.Millisecond)

	now := time.Unix(1700000000, 0).UTC()
	cur := now
	svc.WithClock(func() time.Time { return cur })

	ctx := context.Background()
	f, err := svc.Init(ctx, "ua")
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	cur = now.Add(1 * time.Second)
	if _, err := svc.Poll(ctx, f.PollToken); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expected expired, got %v", err)
	}
	if _, err := svc.BeginGrant(ctx, f.LoginToken); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expected expired begin grant, got %v", err)
	}

	deleted := store.DeleteExpired(ctx, cur)
	if deleted != 1 {
		t.Fatalf("expected 1 expired deleted, got %d", deleted)
	}
}

func TestPollUnknownToken(t *testing.T) {
	svc := NewService(NewMemoryStore())
	if _, err := svc.Poll(context.Background(), "nope"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestGrantInvalidState(t *testing.T) {
	svc := NewService(NewMemoryStore())
	if _, err := svc.Grant(context.Background(), "", "s", "u", "p"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected invalid state on empty token, got %v", err)
	}
	if _, err := svc.Grant(context.Background(), "deadbeef", "s", "u", "p"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestBeginGrantOnceThenLocked(t *testing.T) {
	svc := NewService(NewMemoryStore())
	ctx := context.Background()
	f, err := svc.Init(ctx, "ua")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	g1, err := svc.BeginGrant(ctx, f.LoginToken)
	if err != nil {
		t.Fatalf("begin1: %v", err)
	}
	g2, err := svc.BeginGrant(ctx, f.LoginToken)
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	if g1.StateToken == g2.StateToken {
		t.Fatal("expected new state token on second begin")
	}
	if _, err := svc.LookupState(ctx, g1.StateToken); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("expected old state token invalidated, got %v", err)
	}
	if _, err := svc.LookupState(ctx, g2.StateToken); err != nil {
		t.Fatalf("expected current state token valid, got %v", err)
	}
	if _, err := svc.Grant(ctx, g2.StateToken, "s", "u", "p"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.BeginGrant(ctx, f.LoginToken); !errors.Is(err, ErrFlowConsumed) {
		t.Fatalf("expected consumed after grant, got %v", err)
	}
}

func TestTokenAlphabetOnly(t *testing.T) {
	tok, err := generateAlphaToken(TokenLength)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for i, b := range []byte(tok) {
		ok := false
		for _, a := range []byte(TokenAlphabet) {
			if a == b {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("byte %d (%q) not in alphabet", i, b)
		}
	}
}
