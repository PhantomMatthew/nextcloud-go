package webdav

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

func doRequestBody(h *Handler, method, target string, principal *auth.Principal, headers map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.ContentLength = int64(len(body))
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
	rr := doRequest(h, "DELETE", "/remote.php/dav/files/admin/foo.txt", p, nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != allowedMethods {
		t.Errorf("Allow header = %q, want %q", got, allowedMethods)
	}
}

func TestHandler_GET_NotFound(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "GET", "/remote.php/dav/files/admin/missing.txt", p, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandler_PUT_Create(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, nil, "hello")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if rr.Header().Get("ETag") == "" {
		t.Error("ETag header missing")
	}
	if rr.Header().Get(HeaderOCETag) == "" {
		t.Error("OC-ETag header missing")
	}
	if !strings.HasSuffix(rr.Header().Get(HeaderOCFileID), "oc123abc") {
		t.Errorf("OC-FileId = %q, want suffix oc123abc", rr.Header().Get(HeaderOCFileID))
	}
}

func TestHandler_PUT_Update(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, nil, "v1")
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, nil, "v2-updated")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandler_PUT_ParentMissing(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/nodir/foo.txt", p, nil, "x")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
}

func TestHandler_PUT_IfNoneMatchStarOnExisting(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, nil, "v1")
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{HeaderIfNoneMatch: "*"}, "v2")
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rr.Code)
	}
}

func TestHandler_PUT_IfMatchMismatch(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, nil, "v1")
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{HeaderIfMatch: `"deadbeef"`}, "v2")
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rr.Code)
	}
}

func TestHandler_PUT_OCChunkedNotImplemented(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{HeaderOCChunked: "1"}, "x")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
}

func TestHandler_PUT_XOCMtimeAccepted(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{HeaderOCMtime: "1700000000"}, "x")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if got := rr.Header().Get(HeaderOCMtime); got != "accepted" {
		t.Errorf("X-OC-Mtime = %q, want accepted", got)
	}
}

func TestHandler_GET_AfterPUT(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	body := "round-trip-content"
	putRR := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/r.txt", p, nil, body)
	if putRR.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201", putRR.Code)
	}

	rr := doRequest(h, "GET", "/remote.php/dav/files/admin/r.txt", p, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != body {
		t.Errorf("body = %q, want %q", rr.Body.String(), body)
	}
	if got := rr.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got, len(body))
	}
	if rr.Header().Get("ETag") != putRR.Header().Get("ETag") {
		t.Errorf("ETag mismatch GET=%q PUT=%q", rr.Header().Get("ETag"), putRR.Header().Get("ETag"))
	}
}

func TestHandler_HEAD_AfterPUT(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/r.txt", p, nil, "abc")
	rr := doRequest(h, "HEAD", "/remote.php/dav/files/admin/r.txt", p, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rr.Body.Len())
	}
	if rr.Header().Get("ETag") == "" {
		t.Error("ETag header missing on HEAD")
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
