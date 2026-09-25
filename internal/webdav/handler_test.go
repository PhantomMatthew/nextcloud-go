package webdav

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

func doRequestBody(h *Handler, method, target string, principal *auth.Principal, headers map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
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
	h, err := NewHandler("/remote.php/dav/files/", NewInMemoryFS(), "oc123abc")
	if err != nil {
		panic(err)
	}
	return h
}

func doRequest(h *Handler, method, target string, principal *auth.Principal, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
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

func TestHandler_REPORT_NotSupported(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", DisplayName: "admin", Enabled: true, AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "REPORT", "/remote.php/dav/files/admin/", p, nil, `<c:calendar-query xmlns:c="urn:ietf:params:xml:ns:caldav"/>`)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandler_REPORT_Addressbook_NotSupported(t *testing.T) {
	h, err := NewHandler("/remote.php/dav/addressbooks/users/", NewInMemoryFS(), "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	p := &auth.Principal{UID: "admin", DisplayName: "admin", Enabled: true, AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "REPORT", "/remote.php/dav/addressbooks/users/admin/", p, nil, `<c:addressbook-query xmlns:c="urn:ietf:params:xml:ns:carddav"/>`)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandler_MKCALENDAR_NotSupported(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", DisplayName: "admin", Enabled: true, AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "MKCALENDAR", "/remote.php/dav/files/admin/cal", p, nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandler_CustomDAVHeader(t *testing.T) {
	h := newTestHandler()
	h.DAVHeader = "1, 3, calendar-access, extended-mkcol"
	h.Allow = "OPTIONS, PROPFIND"
	rr := doRequest(h, "OPTIONS", "/remote.php/dav/files/admin/", nil, nil)
	if got := rr.Header().Get("DAV"); got != h.DAVHeader {
		t.Errorf("DAV header = %q", got)
	}
	if got := rr.Header().Get("Allow"); got != h.Allow {
		t.Errorf("Allow = %q", got)
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
		`<d:supportedlock>`,
		`<d:lockdiscovery/>`,
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
	rr := doRequest(h, "PATCH", "/remote.php/dav/files/admin/foo.txt", p, nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != allowedMethods {
		t.Errorf("Allow header = %q, want %q", got, allowedMethods)
	}
}

func TestHandler_MKCOL_Create(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "MKCOL", "/remote.php/dav/files/admin/d1/", p, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if rr.Header().Get(HeaderOCETag) == "" {
		t.Error("OC-ETag missing")
	}
	if !strings.HasSuffix(rr.Header().Get(HeaderOCFileID), "oc123abc") {
		t.Errorf("OC-FileId = %q, want oc123abc suffix", rr.Header().Get(HeaderOCFileID))
	}
}

func TestHandler_MKCOL_Conflict_Existing(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequest(h, "MKCOL", "/remote.php/dav/files/admin/d1/", p, nil)
	rr := doRequest(h, "MKCOL", "/remote.php/dav/files/admin/d1/", p, nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandler_MKCOL_ParentMissing(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "MKCOL", "/remote.php/dav/files/admin/missing/sub/", p, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
}

func TestHandler_MKCOL_WithBody_415(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "MKCOL", "/remote.php/dav/files/admin/d1/", p, nil, "<xml/>")
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rr.Code)
	}
}

func TestHandler_MKCOL_NoAuth(t *testing.T) {
	h := newTestHandler()
	rr := doRequest(h, "MKCOL", "/remote.php/dav/files/admin/d1/", nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestHandler_DELETE_File(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/x.txt", p, nil, "x")
	rr := doRequest(h, "DELETE", "/remote.php/dav/files/admin/x.txt", p, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandler_DELETE_NotFound(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "DELETE", "/remote.php/dav/files/admin/missing.txt", p, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandler_DELETE_Root_Forbidden(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "DELETE", "/remote.php/dav/files/admin/", p, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestHandler_MOVE_File(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/a.txt", p, nil, "v")
	rr := doRequest(h, "MOVE", "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		"Destination": "http://example.test/remote.php/dav/files/admin/b.txt",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if rr.Header().Get(HeaderOCETag) == "" {
		t.Error("OC-ETag missing")
	}
}

func TestHandler_MOVE_OverwriteFalse_412(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/a.txt", p, nil, "v1")
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/b.txt", p, nil, "v2")
	rr := doRequest(h, "MOVE", "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		"Destination": "http://example.test/remote.php/dav/files/admin/b.txt",
		"Overwrite":   "F",
	})
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rr.Code)
	}
}

func TestHandler_MOVE_CrossUser_502(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/a.txt", p, nil, "v")
	rr := doRequest(h, "MOVE", "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		"Destination": "http://example.test/remote.php/dav/files/bob/a.txt",
	})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

func TestHandler_MOVE_MissingDestination_400(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/a.txt", p, nil, "v")
	rr := doRequest(h, "MOVE", "/remote.php/dav/files/admin/a.txt", p, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandler_COPY_File(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/a.txt", p, nil, "v")
	rr := doRequest(h, "COPY", "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		"Destination": "http://example.test/remote.php/dav/files/admin/b.txt",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	getRR := doRequest(h, "GET", "/remote.php/dav/files/admin/a.txt", p, nil)
	if getRR.Code != http.StatusOK {
		t.Errorf("source disappeared after COPY: status = %d", getRR.Code)
	}
}

func TestHandler_COPY_Overwrite(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/a.txt", p, nil, "v1")
	_ = doRequestBody(h, "PUT", "/remote.php/dav/files/admin/b.txt", p, nil, "v2")
	rr := doRequest(h, "COPY", "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		"Destination": "http://example.test/remote.php/dav/files/admin/b.txt",
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
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

// condRecordingFS wraps InMemoryFS with a WriteIf that records the cond it
// received, so tests can assert the handler parsed and forwarded the
// conditional headers intact (ADR-0094).
type condRecordingFS struct {
	*InMemoryFS
	writeCalls   int
	writeIfCalls int
	lastCond     *WriteCond
	writeIfErr   error
}

func (f *condRecordingFS) Write(ctx context.Context, user, p string, r io.Reader, mtime *time.Time) (*Entry, bool, error) {
	f.writeCalls++
	return f.InMemoryFS.Write(ctx, user, p, r, mtime)
}

func (f *condRecordingFS) WriteIf(ctx context.Context, user, p string, r io.Reader, mtime *time.Time, cond *WriteCond) (*Entry, bool, error) {
	f.writeIfCalls++
	f.lastCond = cond
	if f.writeIfErr != nil {
		return nil, false, f.writeIfErr
	}
	return f.InMemoryFS.Write(ctx, user, p, r, mtime)
}

func newCondTestHandler(fake *condRecordingFS) *Handler {
	h, err := NewHandler("/remote.php/dav/files/", fake, "oc123abc")
	if err != nil {
		panic(err)
	}
	return h
}

func TestHandler_PUT_IfETagRoutedToWriteIf(t *testing.T) {
	fake := &condRecordingFS{InMemoryFS: NewInMemoryFS()}
	h := newCondTestHandler(fake)
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{
		HeaderIf:          `</remote.php/dav/files/admin/foo.txt> (["etag123"] <opaquelocktoken:x>)`,
		HeaderIfMatch:     `"abc", W/"def"`,
		HeaderIfNoneMatch: `"zzz"`,
	}, "body")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if fake.writeIfCalls != 1 || fake.writeCalls != 0 {
		t.Fatalf("WriteIf calls = %d, Write calls = %d; want 1/0", fake.writeIfCalls, fake.writeCalls)
	}
	cond := fake.lastCond
	if cond == nil {
		t.Fatal("cond not recorded")
	}
	if !slices.Equal(cond.IfETags, []string{"etag123"}) {
		t.Errorf("IfETags = %v", cond.IfETags)
	}
	if !slices.Equal(cond.IfMatch, []string{"abc", "def"}) {
		t.Errorf("IfMatch = %v", cond.IfMatch)
	}
	if !slices.Equal(cond.IfNoneMatch, []string{"zzz"}) {
		t.Errorf("IfNoneMatch = %v", cond.IfNoneMatch)
	}
}

func TestHandler_PUT_WriteIfPreconditionMaps412(t *testing.T) {
	fake := &condRecordingFS{InMemoryFS: NewInMemoryFS(), writeIfErr: ErrPrecondition}
	h := newCondTestHandler(fake)
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{
		HeaderIf: `(["stale"])`,
	}, "body")
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rr.Code)
	}
}

func TestHandler_PUT_IfLockTokenOnlyUsesPlainWrite(t *testing.T) {
	fake := &condRecordingFS{InMemoryFS: NewInMemoryFS()}
	h := newCondTestHandler(fake)
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{
		HeaderIf: `(<opaquelocktoken:11112222333344445555666677778888>)`,
	}, "body")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if fake.writeCalls != 1 || fake.writeIfCalls != 0 {
		t.Fatalf("Write calls = %d, WriteIf calls = %d; want 1/0 (lock-token-only If: must not trigger the conditional path)", fake.writeCalls, fake.writeIfCalls)
	}
}

func TestHandler_PUT_LegacyFSIfNoneMatchStarOnMissing(t *testing.T) {
	// InMemoryFS has no WriteIf: the legacy Stat+Evaluate branch must let a
	// create-only precondition through.
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	rr := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/new.txt", p, map[string]string{HeaderIfNoneMatch: "*"}, "v1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
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

func TestHandler_PropPatchFavorite(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	putRR := doRequestBody(h, "PUT", "/remote.php/dav/files/admin/foo.txt", p, nil, "hello")
	if putRR.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201", putRR.Code)
	}
	body := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:set><d:prop><oc:favorite>1</oc:favorite></d:prop></d:set></d:propertyupdate>`
	rr := doRequestBody(h, "PROPPATCH", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{"Content-Type": "application/xml"}, body)
	if rr.Code != StatusMultiStatus {
		t.Fatalf("status = %d, want 207 body=%s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()
	if !strings.Contains(got, `<oc:favorite/>`) || !strings.Contains(got, `HTTP/1.1 200 OK`) {
		t.Fatalf("proppatch body=%s", got)
	}
	rr = doRequest(h, "PROPFIND", "/remote.php/dav/files/admin/foo.txt", p, map[string]string{"Depth": "0"})
	if rr.Code != StatusMultiStatus {
		t.Fatalf("propfind status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `<oc:favorite>1</oc:favorite>`) {
		t.Fatalf("propfind missing favorite 1: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `/foo.txt/foo.txt`) {
		t.Fatalf("propfind doubled href: %s", rr.Body.String())
	}
	bad := doRequestBody(h, "PROPPATCH", "/remote.php/dav/files/admin/foo.txt", p, nil, "not-xml")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid xml status = %d", bad.Code)
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

func TestHandler_WebDAVRoot_PROPFIND(t *testing.T) {
	h, err := NewHandler("/remote.php/webdav/", NewInMemoryFS(), "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	h.OwnerUID = func(r *http.Request) string {
		p, ok := auth.UserFromContext(r.Context())
		if !ok {
			return ""
		}
		return p.UID
	}
	h.OwnerName = func(uid string) string { return uid }
	h.Quota = func(_ context.Context, _ string) (int64, int64, bool) {
		return 13, -3, true
	}
	p := &auth.Principal{UID: "admin", DisplayName: "admin", Enabled: true, AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "PROPFIND", "/remote.php/webdav/", p, map[string]string{"Depth": "0"})
	if rr.Code != StatusMultiStatus {
		t.Fatalf("status = %d, want 207, body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, s := range []string{
		`<d:href>/remote.php/webdav/</d:href>`,
		`<oc:owner-id>admin</oc:owner-id>`,
		`<d:quota-used-bytes>13</d:quota-used-bytes>`,
		`<d:quota-available-bytes>-3</d:quota-available-bytes>`,
		`<nc:is-encrypted>false</nc:is-encrypted>`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("body missing %q\nbody=%s", s, body)
		}
	}
}

func TestHandler_WebDAVRoot_Unauthenticated(t *testing.T) {
	h, err := NewHandler("/remote.php/webdav/", NewInMemoryFS(), "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	h.OwnerUID = func(_ *http.Request) string { return "" }
	rr := doRequest(h, "PROPFIND", "/remote.php/webdav/", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestHandler_ParseOwnerPath(t *testing.T) {
	h, err := NewHandler("/remote.php/webdav/", NewInMemoryFS(), "x")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		in      string
		wantSub string
		wantOK  bool
	}{
		{"/remote.php/webdav/", "/", true},
		{"/remote.php/webdav/foo.txt", "/foo.txt", true},
		{"/remote.php/webdav/dir/a", "/dir/a", true},
		{"/remote.php/webdav", "", false},
		{"/other", "", false},
	}
	for _, tc := range tests {
		u, s, ok := h.parseOwnerPath(tc.in, "admin")
		if ok != tc.wantOK || (ok && (u != "admin" || s != tc.wantSub)) {
			t.Errorf("parseOwnerPath(%q) = (%q,%q,%v), want (admin,%q,%v)", tc.in, u, s, ok, tc.wantSub, tc.wantOK)
		}
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
	h, err := NewHandler("/foo", NewInMemoryFS(), "x")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if h.Prefix != "/foo/" {
		t.Errorf("Prefix = %q, want %q", h.Prefix, "/foo/")
	}
}

func TestHandler_NewHandler_ErrorOnBadPrefix(t *testing.T) {
	for _, prefix := range []string{"", "foo"} {
		h, err := NewHandler(prefix, NewInMemoryFS(), "x")
		if !errors.Is(err, ErrInvalidPrefix) {
			t.Errorf("NewHandler(%q) error = %v, want ErrInvalidPrefix", prefix, err)
		}
		if h != nil {
			t.Errorf("NewHandler(%q) returned non-nil handler on error", prefix)
		}
	}
}

func TestHandler_AssembleMoveDotFile(t *testing.T) {
	h, err := NewHandler("/remote.php/dav/uploads/", NewInMemoryFS(), "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	var gotTID, gotDest string
	h.Assemble = func(_ context.Context, srcUser, transferID, destUser, destPath string, overwrite bool, mtime *time.Time, checksum, ifHeader string) (*Entry, bool, error) {
		if srcUser != "alice" || destUser != "alice" || !overwrite {
			t.Errorf("users overwrite = %s %s %v", srcUser, destUser, overwrite)
		}
		gotTID = transferID
		gotDest = destPath
		if checksum != "SHA256:ab" || ifHeader == "" || mtime == nil {
			t.Errorf("meta checksum=%q if=%q mtime=%v", checksum, ifHeader, mtime)
		}
		return &Entry{Path: destPath, ETag: "etag1", NumericID: 9, ModTime: time.Unix(1, 0).UTC()}, true, nil
	}
	p := &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "MOVE", "/remote.php/dav/uploads/alice/tid1/.file", p, map[string]string{
		HeaderDestination: "https://cloud.example.com/remote.php/dav/files/alice/path/to/file.bin",
		HeaderOCChecksum:  "SHA256:ab",
		HeaderOCMtime:     "1714579200",
		HeaderIf:          `</remote.php/dav/files/alice/path/to/file.bin> (["old"])`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotTID != "tid1" || gotDest != "/path/to/file.bin" {
		t.Fatalf("tid=%q dest=%q", gotTID, gotDest)
	}
	if rr.Header().Get(HeaderOCFileID) == "" || rr.Header().Get(HeaderOCETag) == "" {
		t.Fatalf("missing headers %v", rr.Header())
	}
}

func TestHandler_RestoreMove(t *testing.T) {
	h, err := NewHandler("/remote.php/dav/trashbin/", NewInMemoryFS(), "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	var gotLoc, gotDest string
	h.Restore = func(_ context.Context, srcUser, locationID, destUser, destPath string, overwrite bool) (*Entry, bool, error) {
		if srcUser != "alice" || destUser != "alice" || !overwrite {
			t.Errorf("users overwrite = %s %s %v", srcUser, destUser, overwrite)
		}
		gotLoc = locationID
		gotDest = destPath
		return &Entry{Path: "/assembled.bin", ETag: "etag1", NumericID: 9, ModTime: time.Unix(1, 0).UTC()}, true, nil
	}
	p := &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "MOVE", "/remote.php/dav/trashbin/alice/trash/assembled.bin.d1746100800", p, map[string]string{
		HeaderDestination: "https://cloud.example.com/remote.php/dav/trashbin/alice/restore/assembled.bin.d1746100800",
		HeaderOverwrite:   "T",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotLoc != "assembled.bin.d1746100800" || gotDest != "" {
		t.Fatalf("loc=%q dest=%q", gotLoc, gotDest)
	}

	rr = doRequest(h, "MOVE", "/remote.php/dav/trashbin/alice/trash/assembled.bin.d1746100800", p, map[string]string{
		HeaderDestination: "https://cloud.example.com/remote.php/dav/files/alice/restored.bin",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("files dest status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotDest != "/restored.bin" {
		t.Fatalf("files dest=%q", gotDest)
	}
}

func TestHandler_RestoreVersionMove(t *testing.T) {
	h, err := NewHandler("/remote.php/dav/versions/", NewInMemoryFS(), "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	var gotID, gotRev string
	h.RestoreVersion = func(_ context.Context, srcUser, fileID, revision, destUser string) (*Entry, bool, error) {
		if srcUser != "alice" || destUser != "alice" {
			t.Errorf("users = %s %s", srcUser, destUser)
		}
		gotID = fileID
		gotRev = revision
		return &Entry{Path: "/a.txt", ETag: "etag1", NumericID: 6, ModTime: time.Unix(1, 0).UTC()}, false, nil
	}
	p := &auth.Principal{UID: "alice", AuthMethod: auth.AuthMethodBasic}
	rr := doRequest(h, "MOVE", "/remote.php/dav/versions/alice/versions/6/1746100800", p, map[string]string{
		HeaderDestination: "https://cloud.example.com/remote.php/dav/versions/alice/restore",
		HeaderOverwrite:   "T",
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotID != "6" || gotRev != "1746100800" {
		t.Fatalf("id=%q rev=%q", gotID, gotRev)
	}
}

func TestHandler_ParseFilesDestination(t *testing.T) {
	h := &Handler{FilesPrefix: "/remote.php/dav/files/"}
	user, sub, err := h.parseFilesDestination("/remote.php/dav/files/alice/foo.bin")
	if err != nil || user != "alice" || sub != "/foo.bin" {
		t.Fatalf("got %s %s %v", user, sub, err)
	}
}

func TestHandler_LockUnlock(t *testing.T) {
	h := newTestHandler()
	p := &auth.Principal{UID: "admin", AuthMethod: auth.AuthMethodBasic}
	put := doRequestBody(h, http.MethodPut, "/remote.php/dav/files/admin/a.txt", p, nil, "hello")
	if put.Code != http.StatusCreated {
		t.Fatalf("put status=%d", put.Code)
	}
	lockBody := `<?xml version="1.0"?><d:lockinfo xmlns:d="DAV:"><d:lockscope><d:exclusive/></d:lockscope><d:locktype><d:write/></d:locktype><d:owner>admin</d:owner></d:lockinfo>`
	locked := doRequestBody(h, "LOCK", "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		HeaderDepth:   "0",
		HeaderTimeout: "Second-1800",
	}, lockBody)
	if locked.Code != http.StatusOK {
		t.Fatalf("lock status=%d body=%s", locked.Code, locked.Body.String())
	}
	token := locked.Header().Get(HeaderLockToken)
	if token == "" || !strings.Contains(locked.Body.String(), "<d:lockdiscovery>") {
		t.Fatalf("lock token/body missing token=%q body=%s", token, locked.Body.String())
	}
	denied := doRequestBody(h, http.MethodPut, "/remote.php/dav/files/admin/a.txt", p, nil, "nope")
	if denied.Code != http.StatusLocked {
		t.Fatalf("put without if status=%d", denied.Code)
	}
	okPut := doRequestBody(h, http.MethodPut, "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		HeaderIf: "(" + token + ")",
	}, "next")
	if okPut.Code != http.StatusNoContent {
		t.Fatalf("put with if status=%d", okPut.Code)
	}
	shared := `<?xml version="1.0"?><d:lockinfo xmlns:d="DAV:"><d:lockscope><d:shared/></d:lockscope><d:locktype><d:write/></d:locktype></d:lockinfo>`
	rr := doRequestBody(h, "LOCK", "/remote.php/dav/files/admin/a.txt", p, nil, shared)
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusLocked {
		t.Fatalf("shared lock status=%d", rr.Code)
	}
	inf := doRequestBody(h, "LOCK", "/remote.php/dav/files/admin/a.txt", p, map[string]string{HeaderDepth: "infinity"}, lockBody)
	if inf.Code != http.StatusLocked && inf.Code != http.StatusOK {
		t.Fatalf("depth infinity status=%d", inf.Code)
	}
	missing := doRequest(h, "UNLOCK", "/remote.php/dav/files/admin/a.txt", p, nil)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("unlock missing header status=%d", missing.Code)
	}
	wrong := doRequest(h, "UNLOCK", "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		HeaderLockToken: "<opaquelocktoken:deadbeefdeadbeefdeadbeefdeadbeef>",
	})
	if wrong.Code != http.StatusConflict {
		t.Fatalf("unlock wrong token status=%d", wrong.Code)
	}
	unlocked := doRequest(h, "UNLOCK", "/remote.php/dav/files/admin/a.txt", p, map[string]string{
		HeaderLockToken: token,
	})
	if unlocked.Code != http.StatusNoContent {
		t.Fatalf("unlock status=%d", unlocked.Code)
	}
}
