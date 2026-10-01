package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
)

// TestWOPIRoutes pin the office.enabled mount gate (ADR-0106): with the flag
// on, the mint route is session-auth-guarded (401 unauthenticated proves the
// mount) and the Collabora callbacks answer their own token check (401, not
// the CSRF chain's 412 — the path bypass must hold for cookie-less Collabora
// POSTs); with the flag off nothing mounts under the richdocuments paths.
func TestWOPIRoutes(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	newApp := func(t *testing.T, officeEnabled bool) *App {
		t.Helper()
		cfg := DevConfig()
		cfg.Database.DSN = "file:" + t.Name() + "?mode=memory&cache=shared"
		cfg.Storage.Backends = map[string]config.BackendConfig{
			"local": {Type: "localfs", Root: t.TempDir()},
		}
		cfg.Office.Enabled = officeEnabled
		if officeEnabled {
			cfg.Office.CollaboraURL = "https://collabora.example.com"
		}
		a, err := New(ctx, cfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close(ctx) })
		return a
	}

	on := newApp(t, true)
	if on.wopiSvc == nil {
		t.Fatal("office.enabled: wopiSvc is nil")
	}
	if on.wopiDisc == nil {
		t.Fatal("office.enabled: wopiDisc is nil")
	}
	for _, path := range []string{
		"/index.php/apps/richdocuments/wopi/token?fileId=1",
		"/index.php/apps/richdocuments/wopi/files/1",
		"/index.php/apps/richdocuments/index?fileId=1",
	} {
		rr := httptest.NewRecorder()
		on.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s: status = %d, want 401 (mounted)", path, rr.Code)
		}
	}
	// A cookie-less POST to the callbacks must reach the handler's token
	// check (401), not die at the CSRF middleware (412).
	rr := httptest.NewRecorder()
	on.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/index.php/apps/richdocuments/wopi/files/1/contents", strings.NewReader("x")))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("POST contents unauthenticated: status = %d, want 401 (mounted, CSRF bypassed)", rr.Code)
	}
	// The capabilities payload advertises Collabora editing only when the
	// WOPI host is enabled (fixed block key, exact-match safe).
	rr = httptest.NewRecorder()
	on.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil))
	if !strings.Contains(rr.Body.String(), `"richdocuments"`) {
		t.Error("office enabled: capabilities payload lacks the richdocuments block")
	}

	off := newApp(t, false)
	if off.wopiSvc != nil {
		t.Fatal("office disabled: wopiSvc is non-nil")
	}
	rr = httptest.NewRecorder()
	off.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/apps/richdocuments/wopi/token?fileId=1", nil))
	if rr.Code == http.StatusUnauthorized {
		t.Error("office disabled: mint route answered 401 (mounted?)")
	}
	rr = httptest.NewRecorder()
	off.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/apps/richdocuments/wopi/files/1", nil))
	if rr.Code == http.StatusUnauthorized {
		t.Error("office disabled: callback route answered 401 (mounted?)")
	}
	rr = httptest.NewRecorder()
	off.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/apps/richdocuments/index?fileId=1", nil))
	if rr.Code == http.StatusUnauthorized {
		t.Error("office disabled: viewer route answered 401 (mounted?)")
	}
	rr = httptest.NewRecorder()
	off.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil))
	if strings.Contains(rr.Body.String(), `"richdocuments"`) {
		t.Error("office disabled: capabilities payload carries the richdocuments block")
	}
}

// TestWOPITokenKeysWiring pins the ADR-0107 seam: with office enabled, the
// wopi service's Keys is the key resolver exactly when encryption with
// per-user keys is on (widened only when non-nil — a typed nil would make
// every mint/callback dispatch to a nil receiver); encryption off leaves
// Keys nil (keyless mints, store-only GC).
func TestWOPITokenKeysWiring(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	newApp := func(t *testing.T, dsn string, encryption bool) *App {
		t.Helper()
		cfg := DevConfig()
		cfg.Database.DSN = dsn
		cfg.Storage.Backends = map[string]config.BackendConfig{
			"local": {Type: "localfs", Root: t.TempDir()},
		}
		cfg.Office.Enabled = true
		cfg.Office.CollaboraURL = "https://collabora.example.com"
		if encryption {
			cfg.Encryption.Enabled = true
			cfg.Encryption.MasterKeyPath = writeTestMasterKey(t)
			cfg.Encryption.PerUserKeys = true
		}
		a, err := New(ctx, cfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close(ctx) })
		return a
	}

	enc := newApp(t, "file:"+t.Name()+"-enc?mode=memory&cache=shared", true)
	if enc.wopiSvc == nil {
		t.Fatal("office enabled: wopiSvc is nil")
	}
	if enc.keyResolver == nil {
		t.Fatal("encryption + per-user keys: keyResolver is nil")
	}
	if enc.wopiSvc.Keys == nil {
		t.Error("office + encryption: wopiSvc.Keys not wired (ADR-0107)")
	}

	plain := newApp(t, "file:"+t.Name()+"-plain?mode=memory&cache=shared", false)
	if plain.wopiSvc == nil {
		t.Fatal("office enabled: wopiSvc is nil")
	}
	if plain.keyResolver != nil {
		t.Fatal("encryption off: keyResolver is non-nil")
	}
	if plain.wopiSvc.Keys != nil {
		t.Error("office without encryption: wopiSvc.Keys wired, want nil")
	}
}
