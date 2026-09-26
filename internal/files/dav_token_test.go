package files_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// TestAppPasswordTokenWrapEndToEnd pins ADR-0102 through the real stack —
// app-password verifier → principal.UnlockedKey → resolveIdentity box path:
// an enrolled user's file GETs 200 with a WRAPPED app password, 403 with a
// bare one, and 200 again after revoke + re-issue with a wrap.
func TestAppPasswordTokenWrapEndToEnd(t *testing.T) {
	ctx := context.Background()
	env := newPWEnv(t, "alice")
	env.write(t, "/a.txt", "v3 hello")
	aliceKey := env.login(t, "alice", "alice-pw")

	const secret = "e2e-secret"
	store := auth.NewSQLStore(env.db)
	store.TokenKeys = env.res
	verifier := auth.NewAppPasswordVerifier(store, secret)
	verifier.Keys = env.res

	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	get := func(raw string) (*httptest.ResponseRecorder, *auth.Principal) {
		t.Helper()
		p, err := verifier.Verify(ctx, "alice", raw)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/remote.php/dav/files/alice/a.txt", nil)
		req = req.WithContext(auth.WithUser(req.Context(), p))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr, p
	}
	issue := func(wrap bool) string {
		t.Helper()
		raw, tok, err := auth.IssueAppPassword(ctx, store, secret, "alice", "alice", "dav client", auth.TokenTypePermanent)
		if err != nil {
			t.Fatal(err)
		}
		if wrap {
			if err := env.res.WrapKeyForToken(ctx, tok.ID, raw, aliceKey); err != nil {
				t.Fatal(err)
			}
		}
		return raw
	}

	// A token carrying a key wrap: the verifier attaches the key and the
	// enrolled file reads.
	rawWrapped := issue(true)
	rr, p := get(rawWrapped)
	if p.UnlockedKey == nil {
		t.Fatal("wrapped token did not attach the unlocked key")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("GET with wrapped token = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "v3 hello" {
		t.Errorf("GET body = %q", got)
	}

	// A token WITHOUT a wrap (pre-enrollment / imported shape): the request
	// authenticates but locks per file.
	rawBare := issue(false)
	rr, p = get(rawBare)
	if p.UnlockedKey != nil {
		t.Fatal("bare token attached a key")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("GET with bare token = %d, want 403", rr.Code)
	}

	// Revoke the wrapped token: the row and its wrap are gone (the resolver
	// hook fired through the store), and the token no longer verifies.
	if err := auth.RevokeAppPassword(ctx, store, secret, rawWrapped); err != nil {
		t.Fatal(err)
	}
	var wraps int64
	if err := env.db.QueryRow(ctx, `SELECT COUNT(*) FROM app_token_keys`).Scan(&wraps); err != nil {
		t.Fatal(err)
	}
	if wraps != 0 {
		t.Errorf("app_token_keys rows after revoke = %d, want 0 (cryptographic revocation)", wraps)
	}
	if _, err := verifier.Verify(ctx, "alice", rawWrapped); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("revoked token verify = %v, want ErrInvalidCredentials", err)
	}

	// Re-issue with a wrap: the client works again.
	rawReissued := issue(true)
	rr, _ = get(rawReissued)
	if rr.Code != http.StatusOK || rr.Body.String() != "v3 hello" {
		t.Errorf("GET with re-issued wrapped token = %d %q, want 200", rr.Code, rr.Body.String())
	}
}
