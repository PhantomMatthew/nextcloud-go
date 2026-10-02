package app

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/mail"
	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// fakeIMAPServer speaks just enough IMAP4rev1 (greeting, CAPABILITY, LOGIN
// OK, LOGOUT) over a loopback listener for the mail wiring tests: the real
// imap.Client verifies accounts against it.
type fakeIMAPServer struct {
	ln net.Listener
	wg sync.WaitGroup
}

func newFakeIMAPServer(t *testing.T) *fakeIMAPServer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIMAPServer{ln: ln}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeIMAPServer) addr() string { return f.ln.Addr().String() }

func (f *fakeIMAPServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	_, _ = io.WriteString(conn, "* OK fake IMAP4rev1 ready\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		verb, _, _ := strings.Cut(rest, " ")
		switch verb {
		case "CAPABILITY":
			_, _ = io.WriteString(conn, "* CAPABILITY IMAP4rev1\r\n"+tag+" OK capability done\r\n")
		case "LOGIN":
			_, _ = io.WriteString(conn, tag+" OK logged in\r\n")
		case "LOGOUT":
			_, _ = io.WriteString(conn, "* BYE bye\r\n"+tag+" OK logout done\r\n")
			return
		default:
			_, _ = io.WriteString(conn, tag+" BAD unsupported\r\n")
		}
	}
}

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
	// M2 verify-on-create dials before persisting; swap the production
	// (egress-guarded) closure for the scripted fake.
	fake := newFakeIMAPServer(t)
	on.mailSvc.DialIMAP = func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error) {
		opts.SSLMode = mail.SSLModeNone
		opts.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, fake.addr())
		}
		return imap.Dial(ctx, opts)
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
	// M6: the unified-search provider list carries "mail" only when enabled.
	providers := httptest.NewRequestWithContext(ctx, http.MethodGet, "/ocs/v2.php/search/providers?format=json", nil)
	providers.SetBasicAuth("admin", "admin")
	rr = httptest.NewRecorder()
	on.Handler().ServeHTTP(rr, providers)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"id":"mail"`) {
		t.Errorf("mail enabled: providers list = %d %s", rr.Code, rr.Body.String())
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
	providersOff := httptest.NewRequestWithContext(ctx, http.MethodGet, "/ocs/v2.php/search/providers?format=json", nil)
	providersOff.SetBasicAuth("admin", "admin")
	rr = httptest.NewRecorder()
	off.Handler().ServeHTTP(rr, providersOff)
	if strings.Contains(rr.Body.String(), `"id":"mail"`) {
		t.Error("mail disabled: providers list carries the mail provider")
	}
}

// TestMailRoutesEgressGuard pins the M2 production wiring (ADR-0108 §4):
// the verify-on-create dial runs through the ADR-0057 egress guard, so a
// loopback IMAP host is refused (400 "cannot connect to IMAP server",
// nothing stored) until mail.egress_allow_private covers it — then the real
// client verifies against the loopback server and the account persists.
func TestMailRoutesEgressGuard(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := newFakeIMAPServer(t)
	host, port, _ := strings.Cut(fake.addr(), ":")
	newApp := func(t *testing.T, allow []string) *App {
		t.Helper()
		cfg := DevConfig()
		cfg.Database.DSN = "file:" + t.Name() + "?mode=memory&cache=shared"
		cfg.Storage.Backends = map[string]config.BackendConfig{
			"local": {Type: "localfs", Root: t.TempDir()},
		}
		cfg.Mail.Enabled = true
		cfg.Mail.EgressAllowPrivate = allow
		a, err := New(ctx, cfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close(ctx) })
		return a
	}
	createBody := `{"emailAddress":"admin@example.com","imapHost":"` + host + `","imapPort":` + port +
		`,"imapSslMode":"none","imapUser":"u","imapPassword":"guard-test-pw","smtpHost":"smtp.example.com","smtpPort":25,"smtpUser":"u"}`
	tryCreate := func(t *testing.T, a *App) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
		t.Helper()
		create := httptest.NewRequestWithContext(ctx, http.MethodPost, "/apps/mail/api/accounts", strings.NewReader(createBody))
		create.Header.Set("Content-Type", "application/json")
		create.Header.Set("OCS-APIRequest", "true")
		create.SetBasicAuth("admin", "admin")
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, create)
		list := httptest.NewRequestWithContext(ctx, http.MethodGet, "/apps/mail/api/accounts", nil)
		list.SetBasicAuth("admin", "admin")
		lr := httptest.NewRecorder()
		a.Handler().ServeHTTP(lr, list)
		return rr, lr
	}

	// Default: the guard refuses the loopback dial — a distinct 400, no row.
	guarded := newApp(t, nil)
	rr, lr := tryCreate(t, guarded)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "cannot connect to IMAP server") {
		t.Errorf("loopback create: status = %d body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "guard-test-pw") {
		t.Error("error response leaks the password")
	}
	if strings.TrimSpace(lr.Body.String()) != "[]" {
		t.Errorf("a guard-refused verification persisted a row: %s", lr.Body.String())
	}

	// Allowlisted: the same dial passes the guard and verifies for real.
	open := newApp(t, []string{"127.0.0.0/8"})
	rr, lr = tryCreate(t, open)
	if rr.Code != http.StatusCreated {
		t.Errorf("allowlisted loopback create: status = %d body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(lr.Body.String(), `"imapHost":"127.0.0.1"`) {
		t.Errorf("allowlisted list = %s", lr.Body.String())
	}
}
