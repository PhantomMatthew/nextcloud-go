package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// sweepSpy records NameSweepRunner calls: the uid and the principal the
// handler attached to the sweep ctx (the unlocked key must ride along for
// enrolled users, ADR-0104 phase 4).
type sweepSpy struct {
	calls   []sweepCall
	failErr error
}

type sweepCall struct {
	uid      string
	unlocked []byte
	enabled  bool
}

func (s *sweepSpy) run(ctx context.Context, uid string) error {
	sc := sweepCall{uid: uid}
	if p, ok := auth.UserFromContext(ctx); ok && p != nil {
		sc.unlocked = p.UnlockedKey
		sc.enabled = p.Enabled
	}
	s.calls = append(s.calls, sc)
	return s.failErr
}

// TestBrowserLoginNameSweepHook pins the phase-4 login conversion seam on the
// browser form: a password login runs the sweep with the unlocked key in ctx;
// an app-password login never fires it; a sweep failure is best-effort — the
// login still succeeds.
func TestBrowserLoginNameSweepHook(t *testing.T) {
	rig := newLoginKeysRig(t)
	spy := &sweepSpy{}
	rig.handler.NameSweepRunner = spy.run

	rr := rig.postLogin(t, "wonderland")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("basic login = %d, want 303", rr.Code)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("sweep calls = %d, want 1", len(spy.calls))
	}
	call := spy.calls[0]
	if call.uid != "alice" || !call.enabled {
		t.Errorf("sweep call = %+v", call)
	}
	if string(call.unlocked) != string(rig.keys.priv) {
		t.Errorf("sweep ctx carries %d key bytes, want the unlocked key", len(call.unlocked))
	}

	// App-password form login: no UnlockForLogin, no sweep.
	spy.calls = nil
	rr = rig.postLogin(t, "app-token")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("app-password login = %d, want 303", rr.Code)
	}
	if len(spy.calls) != 0 {
		t.Errorf("sweep fired for an app-password login: %+v", spy.calls)
	}

	// A failing sweep never fails the login.
	spy.calls = nil
	spy.failErr = errors.New("sweep: db gone")
	rr = rig.postLogin(t, "wonderland")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login with failing sweep = %d, want 303", rr.Code)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("sweep calls = %d, want 1", len(spy.calls))
	}
	rig.sessionCookie(t, rr) // the session issued normally
}

// TestLoginV2NameSweepHook pins the mirror seam on the login-v2 grant (the
// desktop client's password login): requireAuth's UnlockForLogin is followed
// by the sweep holding the unlocked key; a sweep failure never fails the
// grant.
func TestLoginV2NameSweepHook(t *testing.T) {
	grant := func(t *testing.T, h *LoginV2) *httptest.ResponseRecorder {
		t.Helper()
		flow, err := h.Service.Init(context.Background(), "test client")
		if err != nil {
			t.Fatal(err)
		}
		st, err := h.Service.BeginGrant(context.Background(), flow.LoginToken)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.NewReader(url.Values{"stateToken": {st.StateToken}}.Encode())
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/grant", body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", basicAuth("alice", "wonderland"))
		rr := httptest.NewRecorder()
		h.HandleGrant(rr, req)
		return rr
	}

	keys := &spyKeys{priv: []byte("0123456789abcdef0123456789abcdef")}
	spy := &sweepSpy{}
	h := newHandler(t, stubIssuer{password: "app-pw-grant"})
	h.Users = stubUserStore{u: &users.User{ID: 7, UID: "alice", DisplayName: "Alice", Enabled: true}}
	h.Sessions = &stubSessionStore{}
	h.Keys = keys
	h.NameSweepRunner = spy.run

	rr := grant(t, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("grant = %d, want 200", rr.Code)
	}
	if len(spy.calls) != 1 || spy.calls[0].uid != "alice" {
		t.Fatalf("sweep calls = %+v", spy.calls)
	}
	if string(spy.calls[0].unlocked) != string(keys.priv) {
		t.Errorf("sweep ctx carries %d key bytes, want the unlocked key", len(spy.calls[0].unlocked))
	}

	// A failing sweep still lets the grant through.
	spy.calls = nil
	spy.failErr = errors.New("sweep: db gone")
	h2 := newHandler(t, stubIssuer{password: "app-pw-grant"})
	h2.Users = stubUserStore{u: &users.User{ID: 7, UID: "alice", DisplayName: "Alice", Enabled: true}}
	h2.Sessions = &stubSessionStore{}
	h2.Keys = keys
	h2.NameSweepRunner = spy.run
	rr = grant(t, h2)
	if rr.Code != http.StatusOK {
		t.Fatalf("grant with failing sweep = %d, want 200", rr.Code)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("sweep calls = %d, want 1", len(spy.calls))
	}
}
