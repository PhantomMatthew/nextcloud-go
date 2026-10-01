package wopi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// viewerEnv adds a fake Collabora discovery server and the viewer handler to
// the e2e harness, with a deterministic token for exact HTML assertions.
type viewerFixture struct {
	env     *wopiEnv
	handler *ViewerHandler
	colla   *httptest.Server
}

func newViewerFixture(t *testing.T) *viewerFixture {
	t.Helper()
	env := newEnv(t)
	env.svc.NewToken = func() string { return "wopitesttoken0000000000000000000001" }
	colla := discoveryServer(t, discoveryFixture, nil)
	t.Cleanup(colla.Close)
	return &viewerFixture{
		env:     env,
		handler: &ViewerHandler{Svc: env.svc, Disc: &Discovery{BaseURL: colla.URL}},
		colla:   colla,
	}
}

func (f *viewerFixture) get(t *testing.T, uid, name, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/index.php/apps/richdocuments/index?"+rawQuery, nil)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, withUser(req, uid, name))
	return rr
}

func TestViewerRendersFormPost(t *testing.T) {
	f := newViewerFixture(t)
	ctx := t.Context()
	if _, _, err := f.env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("content"), nil); err != nil {
		t.Fatal(err)
	}
	id := f.env.fileID(t, "/doc.odt")

	rr := f.get(t, "alice", "Alice", "fileId="+strconv.FormatInt(id, 10))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// The WOPI bootstrap: a form POSTing the minted token into the Collabora
	// iframe, auto-submitted.
	if !strings.Contains(body, `<form id="office_form"`) || !strings.Contains(body, `target="office_frame"`) ||
		!strings.Contains(body, `document.getElementById("office_form").submit()`) {
		t.Errorf("form-post bootstrap missing:\n%s", body)
	}
	if !strings.Contains(body, `name="access_token" value="wopitesttoken0000000000000000000001"`) {
		t.Error("minted token not in the form")
	}
	if !strings.Contains(body, `name="access_token_ttl"`) {
		t.Error("token ttl not in the form")
	}
	// The form action is the discovery edit urlsrc carrying the absolute,
	// URL-encoded WOPISrc for THIS file id.
	wantSrc := "http://example.com" + WopiFilesPrefix + strconv.FormatInt(id, 10)
	wantAction := "http://colla.test/browser/abc123/cool.html?WOPISrc=" + url.QueryEscape(wantSrc)
	if !strings.Contains(body, `action="`+wantAction+`"`) {
		t.Errorf("action missing %q:\n%s", wantAction, body)
	}
	if !strings.Contains(body, "<title>doc.odt - Office</title>") {
		t.Error("document name missing from the title")
	}
}

func TestViewerReadOnlyShareeUsesViewAction(t *testing.T) {
	f := newViewerFixture(t)
	testShareeSetup(t, f.env)
	id := f.env.fileID(t, "/shared/report.odt")

	rr := f.get(t, "bob", "Bob", "fileId="+strconv.FormatInt(id, 10))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	// Bob's share is read-only: the form posts to the discovery VIEW urlsrc.
	if !strings.Contains(rr.Body.String(), `action="http://colla.test/browser/abc123/cool.html?readonly=1&amp;WOPISrc=`) {
		t.Errorf("expected the view action for a read-only sharee:\n%s", rr.Body.String())
	}
}

func TestViewerFailures(t *testing.T) {
	f := newViewerFixture(t)
	ctx := t.Context()
	if _, _, err := f.env.dav.Write(ctx, "alice", "/data.xyz", strings.NewReader("x"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.env.dav.Mkdir(ctx, "alice", "/dir"); err != nil {
		t.Fatal(err)
	}
	xyzID := f.env.fileID(t, "/data.xyz")
	dirID := f.env.fileID(t, "/dir")

	if rr := f.get(t, "alice", "Alice", "fileId="); rr.Code != http.StatusBadRequest {
		t.Errorf("missing fileId = %d, want 400", rr.Code)
	}
	if rr := f.get(t, "alice", "Alice", "fileId=abc"); rr.Code != http.StatusBadRequest {
		t.Errorf("bad fileId = %d, want 400", rr.Code)
	}
	if rr := f.get(t, "alice", "Alice", "fileId=999999"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown fileId = %d, want 404", rr.Code)
	}
	if rr := f.get(t, "alice", "Alice", "fileId="+strconv.FormatInt(dirID, 10)); rr.Code != http.StatusBadRequest {
		t.Errorf("dir fileId = %d, want 400", rr.Code)
	}
	// No discovery action for .xyz.
	if rr := f.get(t, "alice", "Alice", "fileId="+strconv.FormatInt(xyzID, 10)); rr.Code != http.StatusNotFound {
		t.Errorf("no-action ext = %d, want 404", rr.Code)
	}
	// Unauthenticated requests never reach the handler logic.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/index.php/apps/richdocuments/index?fileId=1", nil)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", rr.Code)
	}
}

func TestViewerDiscoveryDown(t *testing.T) {
	f := newViewerFixture(t)
	f.colla.Close() // discovery unreachable
	ctx := t.Context()
	if _, _, err := f.env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("x"), nil); err != nil {
		t.Fatal(err)
	}
	id := f.env.fileID(t, "/doc.odt")
	if rr := f.get(t, "alice", "Alice", "fileId="+strconv.FormatInt(id, 10)); rr.Code != http.StatusBadGateway {
		t.Fatalf("discovery down = %d, want 502", rr.Code)
	}
}

func TestViewerEscapesFileName(t *testing.T) {
	f := newViewerFixture(t)
	ctx := t.Context()
	name := "/<script>alert(1).odt"
	if _, _, err := f.env.dav.Write(ctx, "alice", name, strings.NewReader("x"), nil); err != nil {
		t.Fatal(err)
	}
	id := f.env.fileID(t, name)
	rr := f.get(t, "alice", "Alice", "fileId="+strconv.FormatInt(id, 10))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "<script>alert(1)") {
		t.Error("file name not escaped in the viewer HTML")
	}
}

// testShareeSetup shares /shared read-only from alice to bob (direct row,
// mirroring TestWOPIShareeAccess).
func testShareeSetup(t *testing.T, env *wopiEnv) {
	t.Helper()
	ctx := t.Context()
	if _, err := env.dav.Mkdir(ctx, "alice", "/shared"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(ctx, "alice", "/shared/report.odt", strings.NewReader("shared"), nil); err != nil {
		t.Fatal(err)
	}
	alice, err := env.us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.shares.Store.Insert(ctx, &files.Share{
		OwnerUserID: alice.ID, ShareType: files.ShareTypeUser, Path: "/shared",
		ItemType: "folder", Token: "wopiviewshare00001", Permissions: webdav.PermRead, ShareWith: "bob",
	}); err != nil {
		t.Fatal(err)
	}
}
