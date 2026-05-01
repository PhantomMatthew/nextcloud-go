package ocs

import (
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

func (s *stubIssuer) Issue(r *http.Request, p *auth.Principal) (string, error) {
	s.gotUID = p.UID
	return s.issueToken, s.issueErr
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
	r := httptest.NewRequest(method, "/?format=json", nil)
	if principal != nil {
		r = r.WithContext(auth.WithUser(r.Context(), principal))
	}
	return r
}

func TestGetAppPasswordHandler_Success(t *testing.T) {
	issuer := &stubIssuer{issueToken: "tok-123"}
	h := GetAppPasswordHandler(V2, issuer)
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
	h := GetAppPasswordHandler(V2, issuer)
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
	h := GetAppPasswordHandler(V2, issuer)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/?format=json", nil)

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
	h := GetAppPasswordHandler(V2, issuer)
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
	r := httptest.NewRequest(http.MethodDelete, "/?format=json", nil)

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
