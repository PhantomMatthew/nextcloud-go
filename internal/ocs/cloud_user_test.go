package ocs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

func TestCloudUserHandler_NoPrincipal_401(t *testing.T) {
	cases := []struct {
		name string
		ver  Version
	}{{"v1", V1}, {"v2", V2}}
	for _, tc := range cases {
		ver := tc.ver
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/cloud/user?format=json", nil)
			CloudUserHandler(ver).ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), `"statuscode":997`) {
				t.Fatalf("body missing 997: %s", rr.Body.String())
			}
		})
	}
}

func TestCloudUserHandler_JSON_v1(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/cloud/user?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{
		UID: "alice", DisplayName: "Alice", Enabled: true,
	}))
	CloudUserHandler(V1).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type=%q", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`"statuscode":100`,
		`"id":"alice"`,
		`"display-name":"Alice"`,
		`"enabled":true`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

func TestCloudUserHandler_JSON_v2(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/cloud/user?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{
		UID: "bob", DisplayName: "Bob", Enabled: false,
	}))
	CloudUserHandler(V2).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`"statuscode":200`,
		`"id":"bob"`,
		`"enabled":false`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

func TestCloudUserHandler_XML_v1(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/cloud/user", nil)
	req.Header.Set("Accept", "application/xml")
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{
		UID: "alice", DisplayName: "Alice", Enabled: true,
	}))
	CloudUserHandler(V1).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/xml") && !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("Content-Type=%q", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"<statuscode>100</statuscode>",
		"<id>alice</id>",
		"<display-name>Alice</display-name>",
		"<enabled>1</enabled>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}
