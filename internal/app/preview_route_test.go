package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
)

// TestCorePreviewPrettyURL pins the extensionless preview mounts (ADR-0077):
// the SPA bootstrap advertises modRewriteWorking: true (ADR-0069), so the NC
// web UI builds preview URLs without the /index.php prefix. A 401 (auth
// challenge) proves the route is mounted and auth-guarded; an unmounted path
// would fall through to the SPA/static catch-all instead.
func TestCorePreviewPrettyURL(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-preview-pretty?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Previews.Enabled = true
	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })

	for _, path := range []string{
		"/index.php/core/preview?file=/welcome.txt&x=32&y=32",
		"/index.php/core/preview.png?file=/welcome.txt&x=32&y=32",
		"/core/preview?file=/welcome.txt&x=32&y=32",
		"/core/preview.png?file=/welcome.txt&x=32&y=32",
	} {
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s: status = %d, want 401 (mounted, auth-guarded)", path, rr.Code)
		}
	}

	// Control: a path under /core/ nothing mounts must NOT look like a
	// preview route (no 401).
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/core/definitely-not-mounted", nil))
	if rr.Code == http.StatusUnauthorized {
		t.Error("GET /core/definitely-not-mounted: got 401, want fall-through (no mount)")
	}
}
