package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	sidecarSource = "console.log('source asset with a fair amount of text')"
	sidecarBR     = "\x1b\x07fake-brotli-payload"
	sidecarGZ     = "\x1f\x8bfake-gzip-payload"
)

// newSidecarFixture builds the standard tree plus a hashed bundle with both
// precompressed sidecars, a plain file with only a gzip sidecar, and an
// index.html sidecar that must never be used (the armed injector rewrites
// the shell per session).
func newSidecarFixture(t *testing.T) *StaticUI {
	t.Helper()
	ui := newStaticFixture(t)
	writeFile(t, filepath.Join(ui.Root, "core", "dist", "main-1a2b3c4d.js.br"), sidecarBR)
	writeFile(t, filepath.Join(ui.Root, "core", "dist", "main-1a2b3c4d.js.gz"), sidecarGZ)
	writeFile(t, filepath.Join(ui.Root, "img", "logo.svg.gz"), sidecarGZ)
	writeFile(t, filepath.Join(ui.Root, "index.html.gz"), sidecarGZ)
	return ui
}

func TestSidecarBrotliPreferred(t *testing.T) {
	t.Parallel()
	ui := newSidecarFixture(t)
	w := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{"Accept-Encoding": "gzip, br"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding = %q, want br", got)
	}
	if w.Body.String() != sidecarBR {
		t.Errorf("body = %q, want the brotli sidecar", w.Body.String())
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	// Cache policy still keys to the source (hashed → immutable).
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", got)
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("Content-Type = %q, want the source asset's type", got)
	}
}

func TestSidecarGzipFallback(t *testing.T) {
	t.Parallel()
	ui := newSidecarFixture(t)
	// Only gzip accepted: the .gz sidecar serves even though .br exists.
	w := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{"Accept-Encoding": "gzip"})
	if w.Header().Get("Content-Encoding") != "gzip" || w.Body.String() != sidecarGZ {
		t.Errorf("got encoding=%q body=%q, want gzip sidecar", w.Header().Get("Content-Encoding"), w.Body.String())
	}
	// br accepted but no .br sidecar for this asset: gzip wins.
	w = do(t, ui, http.MethodGet, "/img/logo.svg", map[string]string{"Accept-Encoding": "br, gzip"})
	if w.Header().Get("Content-Encoding") != "gzip" || w.Body.String() != sidecarGZ {
		t.Errorf("got encoding=%q body=%q, want gzip sidecar", w.Header().Get("Content-Encoding"), w.Body.String())
	}
}

func TestSidecarIdentityCases(t *testing.T) {
	t.Parallel()
	ui := newSidecarFixture(t)
	// No Accept-Encoding: identity.
	w := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", nil)
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want identity", got)
	}
	if w.Body.String() != bundleBody {
		t.Errorf("body = %q, want source", w.Body.String())
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("identity response lost Vary: %q", got)
	}
	// q=0 excludes the encoding.
	w = do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{"Accept-Encoding": "br;q=0, gzip;q=0.0"})
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q with q=0, want identity", got)
	}
	// A wildcard alone does not select a sidecar.
	w = do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{"Accept-Encoding": "*"})
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q with wildcard, want identity", got)
	}
}

func TestSidecarConditionalRequests(t *testing.T) {
	t.Parallel()
	ui := newSidecarFixture(t)
	info, err := os.Stat(filepath.Join(ui.Root, "core", "dist", "main-1a2b3c4d.js"))
	if err != nil {
		t.Fatal(err)
	}
	ims := info.ModTime().UTC().Format(http.TimeFormat)
	// The 304 decision keys to the SOURCE modtime, on both representations.
	w := do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{"Accept-Encoding": "br", "If-Modified-Since": ims})
	if w.Code != http.StatusNotModified {
		t.Errorf("sidecar If-Modified-Since = %d, want 304", w.Code)
	}
	w = do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{"If-Modified-Since": ims})
	if w.Code != http.StatusNotModified {
		t.Errorf("identity If-Modified-Since = %d, want 304", w.Code)
	}
	// The source's Last-Modified is advertised on the encoded response.
	w = do(t, ui, http.MethodGet, "/core/dist/main-1a2b3c4d.js", map[string]string{"Accept-Encoding": "br"})
	if got := w.Header().Get("Last-Modified"); got != info.ModTime().UTC().Truncate(time.Second).Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %q, want source modtime %q", got, info.ModTime())
	}
}

func TestSidecarNeverForInjectedShell(t *testing.T) {
	t.Parallel()
	ui := newSidecarFixture(t)
	ui.Shell = stubBootstrap{token: "tok-precomp"}
	w := do(t, ui, http.MethodGet, "/", map[string]string{"Accept-Encoding": "br, gzip"})
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("shell Content-Encoding = %q, want identity (injection must run)", got)
	}
	if !strings.Contains(w.Body.String(), `data-requesttoken="tok-precomp"`) {
		t.Error("shell was not injected — an index.html sidecar must never bypass the injector")
	}
}

func TestSidecarEscapingSymlinkFallsBackToIdentity(t *testing.T) {
	t.Parallel()
	ui := newSidecarFixture(t)
	outside := filepath.Join(t.TempDir(), "payload.gz")
	writeFile(t, outside, sidecarGZ)
	link := filepath.Join(ui.Root, "misc", "data.bin.gz")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	w := do(t, ui, http.MethodGet, "/misc/data.bin", map[string]string{"Accept-Encoding": "gzip"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (asset itself is fine)", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want identity (escaping sidecar ignored)", got)
	}
	if w.Body.String() != "\x00\x01" {
		t.Errorf("body = %q, want the source asset", w.Body.String())
	}
}

func TestSidecarHEAD(t *testing.T) {
	t.Parallel()
	ui := newSidecarFixture(t)
	w := do(t, ui, http.MethodHead, "/core/dist/main-1a2b3c4d.js", map[string]string{"Accept-Encoding": "br"})
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("HEAD Content-Encoding = %q, want br", got)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body = %d bytes, want 0", w.Body.Len())
	}
}

func TestAcceptsEncodingTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header, token string
		want          bool
	}{
		{"gzip, br", "br", true},
		{"gzip, br", "gzip", true},
		{"GZip", "gzip", true},
		{"br;q=0", "br", false},
		{"br;q=0.0", "br", false},
		{"br;q=0.5", "br", true},
		{"br; q=0", "br", false},
		{"br;q=garbage", "br", true}, // unparseable q → q=1
		{"*", "gzip", false},
		{"", "gzip", false},
		{"compress", "gzip", false}, // prefix must not match
		{"xgzip", "gzip", false},
	}
	for _, c := range cases {
		if got := acceptsEncoding(c.header, c.token); got != c.want {
			t.Errorf("acceptsEncoding(%q, %q) = %v, want %v", c.header, c.token, got, c.want)
		}
	}
}
