package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// TestConsoleMounted pins the ADR-0080 mounting: /console is claimed by the
// embedded console (auth + admin gate), never shadowed by the static
// catch-all, and an anonymous browser gets the login-link page rather than
// the SPA shell or an empty WebDAV 401.
func TestConsoleMounted(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-console-route?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	// Enable the static catch-all so the test proves /console stays ahead of
	// it (the router's exact/longest-prefix rules).
	staticRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticRoot, "index.html"), []byte("<html>spa shell</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Web.StaticRoot = staticRoot
	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })

	// A second, non-admin user for the 403 branch.
	hash, err := a.hasher.Hash("bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Users.Create(ctx, &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: hash, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	get := func(path, uid, pass string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if uid != "" {
			req.SetBasicAuth(uid, pass)
		}
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		return rr
	}

	// Anonymous browser: the console's own 401 page, with the login link —
	// proving both the mount and the console failure writer.
	rr := get("/console", "", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /console = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("anonymous /console Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rr.Body.String(), "/index.php/login") {
		t.Errorf("anonymous /console body must link the login page: %q", rr.Body.String())
	}

	// Anonymous api fetch: JSON 401 (the console.js layer renders it).
	rr = get("/console/api/status", "", "")
	if rr.Code != http.StatusUnauthorized || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Errorf("anonymous api = %d %q, want 401 JSON", rr.Code, rr.Header().Get("Content-Type"))
	}

	// The bootstrap admin lands in the admin group (ADR-0080): basic auth
	// reaches the page and the api.
	rr = get("/console", "admin", "admin")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ncgo admin console") {
		t.Errorf("admin /console = %d", rr.Code)
	}
	rr = get("/console/api/status", "admin", "admin")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"instanceID":"ocdev00001"`) {
		t.Errorf("admin api status = %d %s", rr.Code, rr.Body.String())
	}

	// Authenticated but not in the admin group: 403.
	rr = get("/console", "bob", "bob")
	if rr.Code != http.StatusForbidden {
		t.Errorf("bob /console = %d, want 403", rr.Code)
	}
	rr = get("/console/api/users", "bob", "bob")
	if rr.Code != http.StatusForbidden {
		t.Errorf("bob api users = %d, want 403", rr.Code)
	}

	// Unsafe methods are 405 at the router with the GET/HEAD Allow.
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/console/api/jobs", nil)
	rr = httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /console/api/jobs = %d Allow %q, want 405 GET, HEAD", rr.Code, rr.Header().Get("Allow"))
	}

	// Control: the static catch-all still owns unclaimed extensionless paths.
	rr = get("/apps/dashboard", "", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "spa shell") {
		t.Errorf("catch-all control = %d, want SPA shell 200", rr.Code)
	}
}
