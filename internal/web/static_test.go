package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
)

const (
	indexBody  = "<!doctype html><html><head><title>ncgo</title></head><body><p>ncgo</p></body></html>"
	bundleBody = "console.log('bundle')"
	canaryBody = "outside the root"
)

// newStaticFixture builds a fixture tree:
//
//	<tmp>/canary.txt                  (outside the served root)
//	<tmp>/web/index.html
//	<tmp>/web/core/dist/main-1a2b3c4d.js
//	<tmp>/web/img/logo.svg
//	<tmp>/web/misc/data.bin
//	<tmp>/web/apps/files/             (directory without an index)
func newStaticFixture(t *testing.T) *StaticUI {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "web")
	writeFile(t, filepath.Join(tmp, "canary.txt"), canaryBody)
	writeFile(t, filepath.Join(root, "index.html"), indexBody)
	writeFile(t, filepath.Join(root, "core", "dist", "main-1a2b3c4d.js"), bundleBody)
	writeFile(t, filepath.Join(root, "img", "logo.svg"), "<svg/>")
	writeFile(t, filepath.Join(root, "misc", "data.bin"), "\x00\x01")
	if err := os.MkdirAll(filepath.Join(root, "apps", "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	ui, err := NewStaticUI(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ui
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func do(t *testing.T, ui *StaticUI, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	ui.ServeHTTP(w, r)
	return w
}

func TestStaticServesIndexAtRoot(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	w := do(t, ui, http.MethodGet, "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET / = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if w.Body.String() != indexBody {
		t.Errorf("body = %q", w.Body.String())
	}
	if w.Header().Get("Last-Modified") == "" {
		t.Error("Last-Modified missing")
	}
}

func TestStaticServesHashedAssetImmutable(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	w := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if w.Body.String() != bundleBody {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestStaticContentTypes(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	for target, want := range map[string]string{
		"/img/logo.svg":   "image/svg+xml",
		"/misc/data.bin":  "application/octet-stream",
		"/apps/dashboard": "text/html; charset=utf-8", // SPA fallback
	} {
		w := do(t, ui, http.MethodGet, target, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != want {
			t.Errorf("GET %s Content-Type = %q, want %q", target, ct, want)
		}
	}
}

func TestStaticSPAFallback(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	for _, target := range []string{"/apps/dashboard", "/index.php/apps/files", "/apps/files/people"} {
		w := do(t, ui, http.MethodGet, target, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, w.Code)
		}
		if w.Body.String() != indexBody {
			t.Errorf("GET %s body = %q, want SPA shell", target, w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q", target, cc)
		}
	}
}

func TestStaticMissingWithExtensionIs404(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	for _, target := range []string{"/core/dist/missing.js", "/img/nope.png", "/index.php"} {
		w := do(t, ui, http.MethodGet, target, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, w.Code)
		}
	}
}

func TestStaticDirWithoutIndexIs404(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	for _, target := range []string{"/apps/files/", "/apps/files"} {
		w := do(t, ui, http.MethodGet, target, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, w.Code)
		}
	}
}

func TestStaticTraversalNeverLeavesRoot(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	for _, target := range []string{
		"/../canary.txt",
		"/%2e%2e/canary.txt",
		"/core/../../../canary.txt",
		"/..",
	} {
		w := do(t, ui, http.MethodGet, target, nil)
		if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 404/400", target, w.Code)
		}
		if strings.Contains(w.Body.String(), canaryBody) {
			t.Errorf("GET %s served the canary outside the root", target)
		}
	}
}

func TestStaticSymlinkEscapeIs404(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	link := filepath.Join(ui.Root, "linked-canary.txt")
	if err := os.Symlink(filepath.Join(filepath.Dir(ui.Root), "canary.txt"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	w := do(t, ui, http.MethodGet, "/linked-canary.txt", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET symlink escape = %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), canaryBody) {
		t.Error("symlink escape served the canary")
	}
}

func TestStaticIfModifiedSince(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	first := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", nil)
	lm := first.Header().Get("Last-Modified")
	if lm == "" {
		t.Fatal("Last-Modified missing")
	}
	second := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{"If-Modified-Since": lm})
	if second.Code != http.StatusNotModified {
		t.Errorf("If-Modified-Since = %d, want 304", second.Code)
	}
	stale := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{
		"If-Modified-Since": time.Now().Add(-24 * time.Hour).UTC().Format(http.TimeFormat),
	})
	if stale.Code != http.StatusOK {
		t.Errorf("stale If-Modified-Since = %d, want 200", stale.Code)
	}
}

func TestStaticHead(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	w := do(t, ui, http.MethodHead, "/core/dist/main-1a2b3c4d.js", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body = %d bytes", w.Body.Len())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestStaticMethodNotAllowed(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := do(t, ui, m, "/", nil)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, w.Code)
		}
		if allow := w.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s Allow = %q", m, allow)
		}
	}
}

func TestNewStaticUIValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewStaticUI("", nil); err == nil {
		t.Error("empty root: expected error")
	}
	if _, err := NewStaticUI("relative/web", nil); err == nil {
		t.Error("relative root: expected error")
	}
	if _, err := NewStaticUI(filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Error("missing root: expected error")
	}
	file := filepath.Join(t.TempDir(), "file.txt")
	writeFile(t, file, "x")
	if _, err := NewStaticUI(file, nil); err == nil {
		t.Error("file root: expected error")
	}
}

func TestStaticRouterInterplay(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	router := httpx.NewRouter()
	router.Handle(http.MethodGet, "/status.php", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("status"))
	}))
	dav := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("dav"))
	})
	router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/files/", dav)
	router.HandlePrefix(http.MethodGet, "/", ui)
	router.HandlePrefix(http.MethodHead, "/", ui)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/status.php", nil))
	if w.Body.String() != "status" {
		t.Errorf("exact route shadowed by static mount: %q", w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/remote.php/dav/files/alice/x.txt", nil))
	if w.Body.String() != "dav" {
		t.Errorf("prefix route shadowed by static mount: %q", w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/apps/dashboard", nil))
	if w.Code != http.StatusOK || w.Body.String() != indexBody {
		t.Errorf("SPA fallback through router = %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/core/dist/main-1a2b3c4d.js", nil))
	if w.Code != http.StatusOK || w.Body.String() != bundleBody {
		t.Errorf("static asset through router = %d %q", w.Code, w.Body.String())
	}
}

// stubBootstrap injects a fixed token, decoupling the injection mechanics
// tests from sessions (login_test.go covers the session-bound wiring).
type stubBootstrap struct{ token string }

func (s stubBootstrap) RequestToken(http.ResponseWriter, *http.Request) string { return s.token }

func TestStaticShellInjectsRequestToken(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	ui.Shell = stubBootstrap{token: "tok-abc"}
	for _, target := range []string{"/", "/index.html", "/apps/dashboard"} {
		w := do(t, ui, http.MethodGet, target, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<head data-requesttoken="tok-abc">`) {
			t.Errorf("GET %s missing head attribute: %q", target, body)
		}
		if !strings.Contains(body, `<script>window.oc_requesttoken="tok-abc";</script>`) {
			t.Errorf("GET %s missing oc_requesttoken global: %q", target, body)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q", target, cc)
		}
	}
}

func TestStaticShellInjectionDisablesConditionalRequests(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	ui.Shell = stubBootstrap{token: "tok-abc"}
	w := do(t, ui, http.MethodGet, "/", nil)
	if lm := w.Header().Get("Last-Modified"); lm != "" {
		t.Errorf("injected shell must not emit Last-Modified, got %q", lm)
	}
	stale := do(t, ui, http.MethodGet, "/", map[string]string{
		"If-Modified-Since": time.Now().UTC().Format(http.TimeFormat),
	})
	if stale.Code != http.StatusOK {
		t.Errorf("If-Modified-Since on injected shell = %d, want always 200", stale.Code)
	}
	if !strings.Contains(stale.Body.String(), "tok-abc") {
		t.Error("conditional shell response must carry the fresh token")
	}
}

func TestStaticShellInjectionReplacesExistingAttribute(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	writeFile(t, filepath.Join(ui.Root, "index.html"),
		`<!doctype html><html><head data-user="alice" data-requesttoken="stale"><title>x</title></head><body/></html>`)
	ui.Shell = stubBootstrap{token: "fresh"}
	w := do(t, ui, http.MethodGet, "/", nil)
	body := w.Body.String()
	if strings.Contains(body, "stale") {
		t.Errorf("stale token must be replaced: %q", body)
	}
	if got := strings.Count(body, `data-requesttoken="fresh"`); got != 1 {
		t.Errorf("data-requesttoken occurrences = %d, want 1: %q", got, body)
	}
	if !strings.Contains(body, `data-user="alice"`) {
		t.Errorf("other head attributes must survive: %q", body)
	}
}

func TestStaticShellInjectionWithoutHeadTag(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	writeFile(t, filepath.Join(ui.Root, "index.html"), `<!doctype html><title>fragment</title>`)
	ui.Shell = stubBootstrap{token: "tok-frag"}
	w := do(t, ui, http.MethodGet, "/", nil)
	if !strings.HasPrefix(w.Body.String(), `<script>window.oc_requesttoken="tok-frag";</script>`) {
		t.Errorf("headless shell must get the script prepended: %q", w.Body.String())
	}
}

func TestStaticAssetsByteIdenticalWithInjector(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	ui.Shell = stubBootstrap{token: "tok-abc"}
	w := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", nil)
	if w.Body.String() != bundleBody {
		t.Errorf("asset body changed under injection: %q", w.Body.String())
	}
	if w.Header().Get("Last-Modified") == "" {
		t.Error("assets keep Last-Modified")
	}
	second := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js",
		map[string]string{"If-Modified-Since": w.Header().Get("Last-Modified")})
	if second.Code != http.StatusNotModified {
		t.Errorf("asset If-Modified-Since = %d, want 304", second.Code)
	}
}

func TestStaticShellHeadOmitsBody(t *testing.T) {
	t.Parallel()
	ui := newStaticFixture(t)
	ui.Shell = stubBootstrap{token: "tok-abc"}
	w := do(t, ui, http.MethodHead, "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD / = %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD shell body = %d bytes", w.Body.Len())
	}
	if cl := w.Header().Get("Content-Length"); cl == "" || cl == "0" {
		t.Errorf("HEAD shell Content-Length = %q", cl)
	}
}
