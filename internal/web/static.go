package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// staticContentTypes is the explicit extension allowlist for served assets.
// Anything else is served as application/octet-stream.
var staticContentTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".ico":         "image/x-icon",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".js":          "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".mjs":         "text/javascript; charset=utf-8",
	".png":         "image/png",
	".svg":         "image/svg+xml",
	".ttf":         "font/ttf",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".webp":        "image/webp",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
}

// hashedAssetPattern matches the content hash Nextcloud bakes into compiled
// bundle names (e.g. main-1a2b3c4d.js); those files are immutable.
var hashedAssetPattern = regexp.MustCompile(`[-.][0-9a-f]{8,}`)

var (
	shellContentType  = "text/html; charset=utf-8"
	shellCacheControl = "no-cache"
)

// ShellBootstrap resolves the per-request values the SPA shell is injected
// with — the requesttoken (ADR-0064) and the bootstrap state (ADR-0069);
// implementations may set cookies while doing so (anonymous login nonce).
type ShellBootstrap interface {
	Bootstrap(w http.ResponseWriter, r *http.Request) (string, BootstrapState)
}

// StaticUI serves a directory of pre-compiled frontend assets (a Nextcloud
// release web root or a built apps directory) with SPA fallback semantics.
// The zero-value fields are set by NewStaticUI, which validates the root.
type StaticUI struct {
	// Root is the symlink-resolved absolute directory assets are served from.
	Root string
	// IndexFallback is the directory index and SPA fallback document name.
	IndexFallback string
	// Logger receives non-fatal serving diagnostics; nil discards them.
	Logger *slog.Logger
	// Shell, when non-nil, arms bootstrap requesttoken injection into the
	// SPA shell (the root index document, however reached, and SPA fallback
	// responses). Every other asset is served byte-identically (ADR-0064).
	Shell ShellBootstrap
}

// NewStaticUI validates root (must exist, be a directory, absolute) and
// resolves symlinks once so per-request containment checks compare against
// the real path.
func NewStaticUI(root string, logger *slog.Logger) (*StaticUI, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("web: static root must not be empty")
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("web: static root %q must be an absolute path", root)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("web: static root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("web: static root %q is not a directory", root)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("web: static root: %w", err)
	}
	return &StaticUI{Root: resolved, IndexFallback: "index.html", Logger: logger}, nil
}

// ServeHTTP serves GET/HEAD requests for files under Root. Directory
// requests serve <dir>/index.html when present, else 404 (no listing).
// Missing paths without a file extension fall back to the SPA shell
// (Root/index.html); missing paths with an extension 404. Traversal
// attempts and symlink escapes always 404.
func (s *StaticUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fsPath, fallbackOK := s.locate(r.URL.Path)
	if fsPath == "" {
		if fallbackOK && path.Ext(r.URL.Path) == "" {
			s.serveIndex(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, fsPath)
}

// locate maps a URL path to a file inside the root. A non-empty result is
// the file to serve. An empty result with fallbackOK=true means nothing
// matched and the caller may serve the SPA shell; fallbackOK=false is a
// hard 404: traversal attempts, symlink escapes, or directories without an
// index document.
func (s *StaticUI) locate(urlPath string) (string, bool) {
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	for _, seg := range strings.Split(urlPath, "/") {
		if seg == ".." {
			return "", false
		}
	}
	fsPath := filepath.Join(s.Root, filepath.FromSlash(path.Clean(urlPath)))
	if !s.contained(fsPath) {
		return "", false
	}
	info, err := os.Stat(fsPath)
	if err != nil {
		return "", true
	}
	if info.IsDir() {
		idx := filepath.Join(fsPath, s.IndexFallback)
		idxInfo, err := os.Stat(idx)
		if err != nil || idxInfo.IsDir() {
			return "", false
		}
		fsPath = idx
	}
	resolved, err := filepath.EvalSymlinks(fsPath)
	if err != nil {
		return "", true
	}
	if !s.contained(resolved) {
		return "", false
	}
	return fsPath, true
}

func (s *StaticUI) contained(p string) bool {
	return p == s.Root || strings.HasPrefix(p, s.Root+string(filepath.Separator))
}

// serveIndex serves the SPA shell with no-cache semantics; a missing shell
// is a plain 404.
func (s *StaticUI) serveIndex(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, filepath.Join(s.Root, s.IndexFallback))
}

func (s *StaticUI) serveFile(w http.ResponseWriter, r *http.Request, fsPath string) {
	if s.Shell != nil && fsPath == filepath.Join(s.Root, s.IndexFallback) {
		s.serveShell(w, r, fsPath)
		return
	}
	f, err := os.Open(fsPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentTypeFor(info.Name()))
	h.Set("Cache-Control", cacheControlFor(info.Name()))
	// The representation depends on Accept-Encoding whenever a sidecar
	// exists, and caches cannot see the filesystem — Vary always (ADR-0081).
	h.Add("Vary", "Accept-Encoding")
	if enc, sidecar := s.negotiateSidecar(r, fsPath); enc != "" {
		defer func() { _ = sidecar.Close() }()
		h.Set("Content-Encoding", enc)
		// The sidecar inherits the source asset's name and modtime: cache
		// policy and conditional requests key to the content version,
		// which is the source's, not the sidecar file's own timestamps.
		http.ServeContent(w, r, info.Name(), info.ModTime(), sidecar)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// negotiateSidecar picks a precompressed sidecar for fsPath against the
// request's Accept-Encoding (ADR-0081): brotli wins over gzip when the
// client accepts both, and a sidecar is used only when it exists as a
// regular file contained in the root (an escaping symlink falls through
// to identity, never 404 — the asset itself is servable). The injected
// SPA shell never reaches here: its bytes vary per session, so no
// precompressed representation of it can exist.
func (s *StaticUI) negotiateSidecar(r *http.Request, fsPath string) (string, *os.File) {
	for _, cand := range []struct{ enc, suffix string }{{"br", ".br"}, {"gzip", ".gz"}} {
		if !acceptsEncoding(r.Header.Get("Accept-Encoding"), cand.enc) {
			continue
		}
		f, err := os.Open(fsPath + cand.suffix)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			_ = f.Close()
			continue
		}
		resolved, err := filepath.EvalSymlinks(fsPath + cand.suffix)
		if err != nil || !s.contained(resolved) {
			_ = f.Close()
			continue
		}
		return cand.enc, f
	}
	return "", nil
}

// acceptsEncoding reports whether the Accept-Encoding header value lists
// token with a nonzero q (a missing or unparseable q means q=1: an explicit
// token from a hand-rolled client gets the encoding it named). Wildcards
// are deliberately not honored — only an explicit br/gzip token selects a
// sidecar, the same stance as nginx's gzip_static.
func acceptsEncoding(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), token) {
			continue
		}
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok || !strings.EqualFold(k, "q") {
				continue
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				q = f
			}
		}
		return q > 0
	}
	return false
}

// serveShell serves the SPA shell with the bootstrap requesttoken and state
// injected. The injected page varies per session, so conditional-request
// negotiation is disabled outright: no Last-Modified, If-Modified-Since is
// ignored, and every response is a full 200 with no-cache semantics
// (ADR-0064, ADR-0069).
func (s *StaticUI) serveShell(w http.ResponseWriter, r *http.Request, fsPath string) {
	body, err := os.ReadFile(fsPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	token, state := s.Shell.Bootstrap(w, r)
	body = injectShell(body, token, state)
	h := w.Header()
	h.Set("Content-Type", shellContentType)
	h.Set("Cache-Control", shellCacheControl)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func contentTypeFor(name string) string {
	if ct, ok := staticContentTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return ct
	}
	return "application/octet-stream"
}

// cacheControlFor treats content-hashed bundle names as immutable; the SPA
// shell and everything else is revalidated on every load.
func cacheControlFor(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if hashedAssetPattern.MatchString(base) {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}
