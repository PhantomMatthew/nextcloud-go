package webdav

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

type stubVerifier struct {
	user string
	pass string
}

func (s stubVerifier) Verify(_ context.Context, user, password string) (*auth.Principal, error) {
	if user == s.user && password == s.pass {
		return &auth.Principal{UID: user, DisplayName: user, Enabled: true, AuthMethod: auth.AuthMethodBasic}, nil
	}
	return nil, auth.ErrInvalidCredentials
}

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestBasicAuth_NoHeader(t *testing.T) {
	mw := BasicAuth(stubVerifier{user: "alice", pass: "wonderland"})
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/remote.php/dav/files/alice/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != wwwAuthenticateValue {
		t.Fatalf("WWW-Authenticate: got %q want %q", got, wwwAuthenticateValue)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("body must be empty, got %d bytes: %q", rr.Body.Len(), rr.Body.String())
	}
	if called {
		t.Fatal("next handler must not be called")
	}
}

func TestBasicAuth_BadHeader(t *testing.T) {
	mw := BasicAuth(stubVerifier{user: "alice", pass: "wonderland"})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler must not be called")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/remote.php/dav/files/alice/", nil)
	req.Header.Set("Authorization", "Bearer xyz")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rr.Code)
	}
}

func TestBasicAuth_BadCredentials(t *testing.T) {
	mw := BasicAuth(stubVerifier{user: "alice", pass: "wonderland"})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler must not be called")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/remote.php/dav/files/alice/", nil)
	req.Header.Set("Authorization", basicHeader("alice", "wrong"))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != wwwAuthenticateValue {
		t.Fatalf("WWW-Authenticate: got %q want %q", got, wwwAuthenticateValue)
	}
}

func TestBasicAuth_Success_InjectsPrincipal(t *testing.T) {
	mw := BasicAuth(stubVerifier{user: "alice", pass: "wonderland"})
	var seen *auth.Principal
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = auth.UserFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/remote.php/dav/files/alice/", nil)
	req.Header.Set("Authorization", basicHeader("alice", "wonderland"))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204 (auth must pass through)", rr.Code)
	}
	if seen == nil {
		t.Fatal("principal must be injected into context")
	}
	if seen.UID != "alice" {
		t.Fatalf("principal UID: got %q want %q", seen.UID, "alice")
	}
	if seen.AuthMethod != auth.AuthMethodBasic {
		t.Fatalf("AuthMethod: got %q want %q", seen.AuthMethod, auth.AuthMethodBasic)
	}
}

func TestBasicAuth_DistinctFromOCS_NoBody(t *testing.T) {
	mw := BasicAuth(stubVerifier{user: "alice", pass: "wonderland"})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PROPFIND", "/remote.php/dav/files/alice/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "" {
		t.Fatalf("Content-Type must be empty (no XML/JSON body), got %q", ct)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("body must be empty, got %q", rr.Body.String())
	}
}
