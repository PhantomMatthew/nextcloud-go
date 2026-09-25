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
	"github.com/PhantomMatthew/nextcloud-go/internal/notifications"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
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

// TestConsoleNotificationsRoute pins the ADR-0090 wiring: the console
// notifications endpoint reads the real notifications store through the
// mounted router, and a seeded row surfaces with the pinned fields.
func TestConsoleNotificationsRoute(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-console-notifs?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })

	admin, err := a.Users.GetByUID(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	n := &notifications.Notification{
		UserID: admin.ID, App: "files_sharing", UserUID: "admin",
		ObjectType: "share", ObjectID: "ocinternal:7",
		Subject: "You received hello.txt as a share by Bob", ShouldNotify: true,
	}
	if err := a.notifStore.Insert(ctx, n); err != nil {
		t.Fatal(err)
	}

	get := func(uid, pass string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/console/api/notifications", nil)
		if uid != "" {
			req.SetBasicAuth(uid, pass)
		}
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		return rr
	}

	rr := get("admin", "admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("admin api notifications = %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`"user":"admin"`, `"app":"files_sharing"`, `"object_id":"ocinternal:7"`, "hello.txt"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}

	// The gate still applies on the new path: anonymous 401, non-admin 403.
	if rr := get("", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous api notifications = %d, want 401", rr.Code)
	}
	hash, err := a.hasher.Hash("bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Users.Create(ctx, &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: hash, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rr := get("bob", "bob"); rr.Code != http.StatusForbidden {
		t.Errorf("bob api notifications = %d, want 403", rr.Code)
	}
}

// TestConsolePluginsRoute pins the ADR-0091 wiring two ways: with metrics
// disabled (the default) the endpoint answers 503 JSON for an admin and 401
// anonymous; with metrics enabled the seeded registry aggregates through the
// mounted router.
func TestConsolePluginsRouteDisabled(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-console-plugins-off?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	// DevConfig leaves observability.metrics_enabled false, so a.metrics
	// stays nil — the endpoint's 503 path.
	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })

	get := func(uid, pass string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/console/api/plugins", nil)
		if uid != "" {
			req.SetBasicAuth(uid, pass)
		}
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		return rr
	}

	rr := get("admin", "admin")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin api plugins (metrics off) = %d %s, want 503", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"error":"metrics disabled"`) {
		t.Errorf("body = %s", rr.Body.String())
	}
	if rr := get("", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous api plugins = %d, want 401", rr.Code)
	}
}

func TestConsolePluginsRouteEnabled(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-console-plugins-on?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Observability.MetricsEnabled = true
	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })

	// Seed the process-wide registry app.New constructed (in-package access);
	// the console handler must read this same instance.
	a.metrics.IncCounter(observability.MetricPluginHostCallsTotal,
		observability.Label{Name: "plugin", Value: "demo"},
		observability.Label{Name: "function", Value: "cache_get"},
		observability.Label{Name: "result", Value: "ok"})
	a.metrics.ObserveHistogram(observability.MetricPluginHostCallDurationSeconds, 0.01,
		observability.Label{Name: "plugin", Value: "demo"},
		observability.Label{Name: "function", Value: "cache_get"})

	get := func(uid, pass string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/console/api/plugins", nil)
		if uid != "" {
			req.SetBasicAuth(uid, pass)
		}
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		return rr
	}

	rr := get("admin", "admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("admin api plugins = %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`"plugin":"demo"`, `"host_calls":1`, `"host_errors":0`, `"host_call_mean_seconds":0.01`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if rr := get("", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous api plugins = %d, want 401", rr.Code)
	}
}
