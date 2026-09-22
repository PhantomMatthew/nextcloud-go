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
	w.Header().Set("Content-Type", contentTypeFor(info.Name()))
	w.Header().Set("Cache-Control", cacheControlFor(info.Name()))
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
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
