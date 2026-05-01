package webdav

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

func newTestHandler() *Handler {
	return NewHandler("/remote.php/dav/files/", NewInMemoryFS(), "oc123abc")
}

func doRequest(h *Handler, method, target string, principal *auth.Principal, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if principal != nil {
		req = req.WithContext(auth.WithUser(context.Background(), principal))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHandler_OPTIONS(t *testing.T) {
	h := newTestHandler()
	rr := doRequest(h, "OPTIONS", "/remote.php/dav/files/admin/", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("DAV"); got != davCompliance {
		t.Errorf("DAV header = %q, want %q", got, davCompliance)
	}
	if got := rr.Header().Get("Allow"); got != allowedMethods {
		t.Errorf("Allow header = %q, want %q", got, allowedMethods)
	}
}

func TestHandler_PROPFIND_Depth0_Root(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", DisplayName: "admin", Enabled: true, AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "PROPFIND", "/remote.php/dav/files/admin/", p, map[string]string{"Depth": "0"})

	if rr.Code != StatusMultiStatus {
		t.Fatalf("status = %d, want 207, body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeXML {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeXML)
	}
	body := rr.Body.String()
	wantSubs := []string{
		`<d:multistatus`,
		`<d:href>/remote.php/dav/files/admin/</d:href>`,
		`<d:collection/>`,
		`<oc:permissions>RGDNVCK</oc:permissions>`,
		`<oc:id>00000001oc123abc</oc:id>`,
		`<d:status>HTTP/1.1 200 OK</d:status>`,
	}
	for _, s := range wantSubs {
		if !strings.Contains(body, s) {
			t.Errorf("body missing %q\nbody=%s", s, body)
		}
	}
	count := strings.Count(body, "<d:response>")
	if count != 1 {
		t.Errorf("Depth:0 expected 1 response, got %d", count)
	}
}

func TestHandler_PROPFIND_Depth1_Root(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "PROPFIND", "/remote.php/dav/files/admin/", p, map[string]string{"Depth": "1"})

	if rr.Code != StatusMultiStatus {
		t.Fatalf("status = %d, want 207", rr.Code)
	}
	count := strings.Count(rr.Body.String(), "<d:response>")
	if count != 1 {
		t.Errorf("Depth:1 on empty home expected 1 response (root only), got %d", count)
	}
}

func TestHandler_PROPFIND_NoAuth(t *testing.T) {
	h := newTestHandler()
	rr := doRequest(h, "PROPFIND", "/remote.php/dav/files/admin/", nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestHandler_PROPFIND_UserMismatch(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "PROPFIND", "/remote.php/dav/files/bob/", p, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestHandler_PROPFIND_UnknownPath(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "PROPFIND", "/remote.php/dav/files/admin/missing/", p, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != allowedMethods {
		t.Errorf("Allow header = %q, want %q", got, allowedMethods)
	}
}

func TestHandler_GET_NotImplemented(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "GET", "/remote.php/dav/files/admin/foo.txt", p, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no file content yet)", rr.Code)
	}
}

func TestHandler_ParsePath(t *testing.T) {
	h := newTestHandler()
	tests := []struct {
		in       string
		wantUser string
		wantSub  string
		wantOK   bool
	}{
		{"/remote.php/dav/files/admin/", "admin", "/", true},
		{"/remote.php/dav/files/admin", "admin", "/", true},
		{"/remote.php/dav/files/admin/foo/bar.txt", "admin", "/foo/bar.txt", true},
		{"/remote.php/dav/files/", "", "", false},
		{"/other/path", "", "", false},
	}
	for _, tc := range tests {
		u, s, ok := h.parsePath(tc.in)
		if u != tc.wantUser || s != tc.wantSub || ok != tc.wantOK {
			t.Errorf("parsePath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, u, s, ok, tc.wantUser, tc.wantSub, tc.wantOK)
		}
	}
}

func TestHandler_NormalizeDepth(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0", "0"},
		{"1", "1"},
		{"infinity", "1"},
		{"", "1"},
		{"garbage", "1"},
	}
	for _, tc := range tests {
		if got := normalizeDepth(tc.in); got != tc.want {
			t.Errorf("normalizeDepth(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHandler_NewHandler_PrefixNormalization(t *testing.T) {
	h := NewHandler("/foo", NewInMemoryFS(), "x")
	if h.Prefix != "/foo/" {
		t.Errorf("Prefix = %q, want %q", h.Prefix, "/foo/")
	}
}

func TestHandler_NewHandler_PanicOnBadPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on prefix without leading /")
		}
	}()
	_ = NewHandler("foo", NewInMemoryFS(), "x")
}

var _ io.Reader = (*strings.Reader)(nil)
