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

// TestMailRoutes pins the mail.enabled mount gate (ADR-0108): with the flag
// on, the accounts API is session-auth-guarded (401 unauthenticated proves
// the mount) and works end to end with basic credentials, and the
// capabilities payload advertises the "mail" block; with the flag off
// nothing mounts under /apps/mail and the block is absent.
func TestMailRoutes(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	newApp := func(t *testing.T, mailEnabled bool) *App {
		t.Helper()
		cfg := DevConfig()
		cfg.Database.DSN = "file:" + t.Name() + "?mode=memory&cache=shared"
		cfg.Storage.Backends = map[string]config.BackendConfig{
			"local": {Type: "localfs", Root: t.TempDir()},
		}
		cfg.Mail.Enabled = mailEnabled
		a, err := New(ctx, cfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close(ctx) })
		return a
	}

	on := newApp(t, true)
	if on.mailSvc == nil {
		t.Fatal("mail.enabled: mailSvc is nil")
	}
	rr := httptest.NewRecorder()
	on.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/apps/mail/api/accounts", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET accounts anonymous: status = %d, want 401 (mounted)", rr.Code)
	}
	// End to end through the full router with basic credentials: create,
	// then list. The CSRF chain admits the unsafe POST via the repo-standard
	// OCS-APIRequest: true opt-out (basic/app-password API clients carry no
	// requesttoken; session-cookie verbs keep the middleware's 403 check) —
	// and the response never carries the password.
	create := httptest.NewRequestWithContext(ctx, http.MethodPost, "/apps/mail/api/accounts",
		strings.NewReader(`{"emailAddress":"admin@example.com","imapHost":"imap.example.com","imapPort":993,"imapUser":"admin@example.com","imapPassword":"route-test-pw","smtpHost":"smtp.example.com","smtpPort":465,"smtpUser":"admin@example.com"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("OCS-APIRequest", "true")
	create.SetBasicAuth("admin", "admin")
	rr = httptest.NewRecorder()
	on.Handler().ServeHTTP(rr, create)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST accounts: status = %d body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "route-test-pw") {
		t.Error("create response leaks the password")
	}
	list := httptest.NewRequestWithContext(ctx, http.MethodGet, "/apps/mail/api/accounts", nil)
	list.SetBasicAuth("admin", "admin")
	rr = httptest.NewRecorder()
	on.Handler().ServeHTTP(rr, list)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"imapHost":"imap.example.com"`) ||
		strings.Contains(rr.Body.String(), "route-test-pw") {
		t.Errorf("list accounts: status = %d body = %s", rr.Code, rr.Body.String())
	}
	// Capabilities advertise the mail block only when enabled.
	rr = httptest.NewRecorder()
	on.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil))
	if !strings.Contains(rr.Body.String(), `"mail"`) {
		t.Error("mail enabled: capabilities payload lacks the mail block")
	}

	off := newApp(t, false)
	if off.mailSvc != nil {
		t.Fatal("mail disabled: mailSvc is non-nil")
	}
	rr = httptest.NewRecorder()
	off.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/apps/mail/api/accounts", nil))
	if rr.Code == http.StatusUnauthorized {
		t.Error("mail disabled: accounts route answered 401 (mounted?)")
	}
	rr = httptest.NewRecorder()
	off.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(ctx, http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil))
	if strings.Contains(rr.Body.String(), `"mail"`) {
		t.Error("mail disabled: capabilities payload carries the mail block")
	}
}
