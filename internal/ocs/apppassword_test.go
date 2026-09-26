package ocs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

type stubIssuer struct {
	issueToken string
	issueErr   error
	revokeErr  error
	gotRaw     string
	gotUID     string
}

func (s *stubIssuer) Issue(r *http.Request, p *auth.Principal) (string, string, error) {
	s.gotUID = p.UID
	return s.issueToken, "tok-id", s.issueErr
}

func (s *stubIssuer) Revoke(r *http.Request, p *auth.Principal, raw string) error {
	s.gotRaw = raw
	s.gotUID = p.UID
	return s.revokeErr
}

func basicAuthHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func decodeOCS(t *testing.T, body []byte) (status string, statuscode int, data map[string]any) {
	t.Helper()
	var env struct {
		OCS struct {
			Meta struct {
				Status     string `json:"status"`
				StatusCode int    `json:"statuscode"`
			} `json:"meta"`
			Data map[string]any `json:"data"`
		} `json:"ocs"`
	}
	body = []byte(strings.ReplaceAll(string(body), `\/`, `/`))
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nbody=%s", err, string(body))
	}
	return env.OCS.Meta.Status, env.OCS.Meta.StatusCode, env.OCS.Data
}

func newAuthedRequest(method string, principal *auth.Principal) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), method, "/?format=json", nil)
	if principal != nil {
		r = r.WithContext(auth.WithUser(r.Context(), principal))
	}
	return r
}

func TestGetAppPasswordHandler_Success(t *testing.T) {
	issuer := &stubIssuer{issueToken: "tok-123"}
	h := GetAppPasswordHandler(V2, issuer, nil)
	rr := httptest.NewRecorder()
	r := newAuthedRequest(http.MethodGet, &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodBasic})

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	status, code, data := decodeOCS(t, rr.Body.Bytes())
	if status != "ok" || code != StatusOKv2 {
		t.Fatalf("meta: want ok/200, got %s/%d", status, code)
	}
	if data["apppassword"] != "tok-123" {
		t.Fatalf("apppassword: want tok-123, got %v", data["apppassword"])
	}
	if issuer.gotUID != "alice" {
		t.Fatalf("issuer uid: want alice, got %s", issuer.gotUID)
	}
}

func TestGetAppPasswordHandler_ForbiddenWhenAppPassword(t *testing.T) {
	issuer := &stubIssuer{issueToken: "should-not-be-issued"}
	h := GetAppPasswordHandler(V2, issuer, nil)
	rr := httptest.NewRecorder()
	r := newAuthedRequest(http.MethodGet, &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodAppPassword})

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d", rr.Code)
	}
	if issuer.gotUID != "" {
		t.Fatalf("issuer should not be called, got uid=%s", issuer.gotUID)
	}
	status, code, _ := decodeOCS(t, rr.Body.Bytes())
	if status != "failure" || code != http.StatusForbidden {
		t.Fatalf("meta: want failure/403, got %s/%d", status, code)
	}
}

func TestGetAppPasswordHandler_UnauthorizedWhenNoPrincipal(t *testing.T) {
	issuer := &stubIssuer{}
	h := GetAppPasswordHandler(V2, issuer, nil)
	rr := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/?format=json", nil)

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != wwwAuthenticateValue {
		t.Fatalf("WWW-Authenticate: want %q, got %q", wwwAuthenticateValue, got)
	}
}

func TestGetAppPasswordHandler_ServerErrorOnIssueFailure(t *testing.T) {
	issuer := &stubIssuer{issueErr: errors.New("boom")}
	h := GetAppPasswordHandler(V2, issuer, nil)
	rr := httptest.NewRecorder()
	r := newAuthedRequest(http.MethodGet, &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodBasic})

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", rr.Code)
	}
}

func TestDeleteAppPasswordHandler_Success(t *testing.T) {
	issuer := &stubIssuer{}
	h := DeleteAppPasswordHandler(V2, issuer)
	rr := httptest.NewRecorder()
	r := newAuthedRequest(http.MethodDelete, &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodAppPassword})
	r.Header.Set("Authorization", basicAuthHeader("alice", "raw-token-xyz"))

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	if issuer.gotRaw != "raw-token-xyz" {
		t.Fatalf("revoke raw: want raw-token-xyz, got %s", issuer.gotRaw)
	}
	status, code, _ := decodeOCS(t, rr.Body.Bytes())
	if status != "ok" || code != StatusOKv2 {
		t.Fatalf("meta: want ok/200, got %s/%d", status, code)
	}
}

func TestDeleteAppPasswordHandler_ForbiddenWhenBasic(t *testing.T) {
	issuer := &stubIssuer{}
	h := DeleteAppPasswordHandler(V2, issuer)
	rr := httptest.NewRecorder()
	r := newAuthedRequest(http.MethodDelete, &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodBasic})

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d", rr.Code)
	}
	if issuer.gotRaw != "" {
		t.Fatalf("issuer.Revoke should not be called, got raw=%s", issuer.gotRaw)
	}
}

func TestDeleteAppPasswordHandler_UnauthorizedWhenNoPrincipal(t *testing.T) {
	issuer := &stubIssuer{}
	h := DeleteAppPasswordHandler(V2, issuer)
	rr := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/?format=json", nil)

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rr.Code)
	}
}

func TestDeleteAppPasswordHandler_ServerErrorOnRevokeFailure(t *testing.T) {
	issuer := &stubIssuer{revokeErr: errors.New("boom")}
	h := DeleteAppPasswordHandler(V2, issuer)
	rr := httptest.NewRecorder()
	r := newAuthedRequest(http.MethodDelete, &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodAppPassword})
	r.Header.Set("Authorization", basicAuthHeader("alice", "raw"))

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", rr.Code)
	}
}

// spyTokenWrapper records WrapKeyForToken calls (ADR-0102).
type spyTokenWrapper struct {
	calls []wrapCall
	err   error
}

type wrapCall struct {
	id   string
	raw  string
	priv []byte
}

func (s *spyTokenWrapper) WrapKeyForToken(_ context.Context, appPasswordID, tokenRaw string, priv []byte) error {
	s.calls = append(s.calls, wrapCall{id: appPasswordID, raw: tokenRaw, priv: priv})
	return s.err
}

// TestGetAppPasswordHandlerWrapsKeyForToken pins ADR-0102 wrap-at-issuance at
// the OCS endpoint: a SESSION-authenticated request carries the middleware-
// attached unlocked key, so the new token gets its wrap; a wrap failure is a
// loud server error; a keyless principal (basic/bearer) issues without
// wrapping — the pre-enrollment behavior.
func TestGetAppPasswordHandlerWrapsKeyForToken(t *testing.T) {
	priv := []byte("0123456789abcdef0123456789abcdef")
	sessionPrincipal := &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodSession, UnlockedKey: priv}

	t.Run("session principal with unlocked key wraps", func(t *testing.T) {
		issuer := &stubIssuer{issueToken: "tok-123"}
		keys := &spyTokenWrapper{}
		h := GetAppPasswordHandler(V2, issuer, keys)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, newAuthedRequest(http.MethodGet, sessionPrincipal))

		if rr.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d", rr.Code)
		}
		_, _, data := decodeOCS(t, rr.Body.Bytes())
		if data["apppassword"] != "tok-123" {
			t.Fatalf("apppassword: want tok-123, got %v", data["apppassword"])
		}
		if len(keys.calls) != 1 {
			t.Fatalf("wrap calls = %v, want 1", keys.calls)
		}
		got := keys.calls[0]
		if got.id != "tok-id" || got.raw != "tok-123" || string(got.priv) != string(priv) {
			t.Errorf("wrap call = %+v, want (tok-id, tok-123, the unlocked key)", got)
		}
	})

	t.Run("wrap error fails loudly", func(t *testing.T) {
		issuer := &stubIssuer{issueToken: "tok-123"}
		keys := &spyTokenWrapper{err: errors.New("db gone")}
		h := GetAppPasswordHandler(V2, issuer, keys)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, newAuthedRequest(http.MethodGet, sessionPrincipal))

		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("wrap failure: want 500, got %d", rr.Code)
		}
	})

	t.Run("no unlocked key issues without wrapping", func(t *testing.T) {
		issuer := &stubIssuer{issueToken: "tok-123"}
		keys := &spyTokenWrapper{}
		h := GetAppPasswordHandler(V2, issuer, keys)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, newAuthedRequest(http.MethodGet, &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodBasic}))

		if rr.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d", rr.Code)
		}
		_, _, data := decodeOCS(t, rr.Body.Bytes())
		if data["apppassword"] != "tok-123" {
			t.Fatalf("apppassword: want tok-123, got %v", data["apppassword"])
		}
		if len(keys.calls) != 0 {
			t.Errorf("wrap called without an unlocked key: %v", keys.calls)
		}
	})

	t.Run("nil wrapper issues without wrapping", func(t *testing.T) {
		issuer := &stubIssuer{issueToken: "tok-123"}
		h := GetAppPasswordHandler(V2, issuer, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, newAuthedRequest(http.MethodGet, sessionPrincipal))

		if rr.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d", rr.Code)
		}
	})
}
