package ocs

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

type stubVerifier struct {
	user, pass string
}

func (s stubVerifier) Verify(_ context.Context, u, p string) (*auth.Principal, error) {
	if u == s.user && p == s.pass {
		return &auth.Principal{UID: u, DisplayName: u, Enabled: true}, nil
	}
	return nil, errors.New("bad")
}

func basicHeader(u, p string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p))
}

func TestBasicAuth_MissingHeader_401(t *testing.T) {
	cases := []struct {
		name string
		ver  Version
	}{{"v1", V1}, {"v2", V2}}
	for _, tc := range cases {
		ver := tc.ver
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/x?format=json", nil)
			h := BasicAuth(ver, stubVerifier{user: "admin", pass: "admin"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler must not be invoked")
			}))
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401", rr.Code)
			}
			if got := rr.Header().Get("WWW-Authenticate"); got != `Basic realm="Authorisation Required"` {
				t.Fatalf("WWW-Authenticate=%q", got)
			}
			body := rr.Body.String()
			if !strings.Contains(body, `"statuscode":997`) && !strings.Contains(body, `<statuscode>997</statuscode>`) {
				t.Fatalf("body missing statuscode 997: %s", body)
			}
			if !strings.Contains(body, "Current user is not logged in") {
				t.Fatalf("body missing message: %s", body)
			}
		})
	}
}

func TestBasicAuth_BadCreds_401(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", basicHeader("admin", "wrong"))
	h := BasicAuth(V1, stubVerifier{user: "admin", pass: "admin"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler must not be invoked")
	}))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rr.Code)
	}
}

func TestBasicAuth_Valid_PassesPrincipal(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", basicHeader("admin", "admin"))
	var seen *auth.Principal
	h := BasicAuth(V1, stubVerifier{user: "admin", pass: "admin"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.UserFromContext(r.Context())
		if !ok {
			t.Fatal("principal missing from context")
		}
		seen = p
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rr.Code)
	}
	if seen == nil || seen.UID != "admin" {
		t.Fatalf("principal=%+v", seen)
	}
}
