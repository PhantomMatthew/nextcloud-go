package app

import (
	"bufio"
	"bytes"
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
)

// sendSMTPServer is the app-level scripted SMTP server for the M5 wiring
// test: plaintext relay (no AUTH — the account is store-patched to relay
// mode, the only cleartext-legal shape), recording the envelope and the
// DATA payload.
type sendSMTPServer struct {
	ln net.Listener
	wg sync.WaitGroup

	mu       sync.Mutex
	mailFrom string
	rcpts    []string
	data     []byte
}

func newSendSMTPServer(t *testing.T) *sendSMTPServer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &sendSMTPServer{ln: ln}
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

func (f *sendSMTPServer) addr() string { return f.ln.Addr().String() }

func (f *sendSMTPServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	_, _ = io.WriteString(conn, "220 fake ESMTP ready\r\n")
	inData := false
	var data bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				f.mu.Lock()
				f.data = append([]byte(nil), data.Bytes()...)
				f.mu.Unlock()
				_, _ = io.WriteString(conn, "250 2.0.0 queued\r\n")
				continue
			}
			data.WriteString(strings.TrimPrefix(line, ".") + "\r\n")
			continue
		}
		verb, args, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			_, _ = io.WriteString(conn, "250 fake greets you\r\n")
		case "MAIL":
			f.mu.Lock()
			f.mailFrom = args
			f.mu.Unlock()
			_, _ = io.WriteString(conn, "250 2.1.0 sender ok\r\n")
		case "RCPT":
			f.mu.Lock()
			f.rcpts = append(f.rcpts, args)
			f.mu.Unlock()
			_, _ = io.WriteString(conn, "250 2.1.5 recipient ok\r\n")
		case "DATA":
			_, _ = io.WriteString(conn, "354 end with <CR><LF>.<CR><LF>\r\n")
			inData = true
		case "QUIT":
			_, _ = io.WriteString(conn, "221 2.0.0 bye\r\n")
			return
		default:
			_, _ = io.WriteString(conn, "502 5.5.2 unsupported\r\n")
		}
	}
}

func (f *sendSMTPServer) payload() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...)
}

func (f *sendSMTPServer) envelope() (string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mailFrom, append([]string(nil), f.rcpts...)
}

// TestMailSendWiring pins the M5 app wiring (ADR-0108 §1): with
// mail.enabled the Sender is built, POST /apps/mail/api/accounts/{id}/send
// delivers through the full router into the scripted SMTP server, and the
// {path} attachment form reads the sender's file through the REAL davFS
// (the share/ownership-aware seam the wiring installs as ReadFile).
func TestMailSendWiring(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fakeIMAP := newFakeIMAPServer(t)
	smtpSrv := newSendSMTPServer(t)
	imapHost, imapPort, _ := strings.Cut(fakeIMAP.addr(), ":")
	cfg := DevConfig()
	cfg.Database.DSN = "file:" + t.Name() + "?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Mail.Enabled = true
	cfg.Mail.EgressAllowPrivate = []string{"127.0.0.0/8"}
	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })
	if a.mailSender == nil {
		t.Fatal("mail.enabled: mailSender is nil")
	}
	// The production DialSMTP is egress-guarded; re-aim it at the fake (the
	// allowlisted loopback would pass the guard too, but the fake's port is
	// not the account's — same swap pattern as mailSvc.DialIMAP in M2).
	a.mailSender.DialSMTP = func(ctx context.Context, _ mail.SMTPOptions) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", smtpSrv.addr())
	}

	// Create the account through the full router (M2 verify-on-create dials
	// the fake IMAP).
	create := httptest.NewRequestWithContext(ctx, http.MethodPost, "/apps/mail/api/accounts",
		strings.NewReader(`{"emailAddress":"admin@example.com","imapHost":"`+imapHost+`","imapPort":`+imapPort+
			`,"imapSslMode":"none","imapUser":"u","imapPassword":"send-wiring-pw","smtpHost":"smtp.example.com","smtpPort":25,"smtpSslMode":"none","smtpUser":"u"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("OCS-APIRequest", "true")
	create.SetBasicAuth("admin", "admin")
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, create)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: status = %d body = %s", rr.Code, rr.Body.String())
	}
	// The API requires an smtp user, but a user over cleartext (mode none)
	// is refused at send time — patch the row to relay mode (empty user →
	// no AUTH), the only plaintext-legal shape, through the store (the REST
	// validation deliberately forbids empty smtpUser).
	acct, err := a.mailSvc.Get(ctx, "admin", 1)
	if err != nil {
		t.Fatal(err)
	}
	acct.SMTPUser = ""
	if err := a.mailSvc.Store.Update(ctx, acct); err != nil {
		t.Fatal(err)
	}

	// A file in the SENDER's files for the {path} attachment form — written
	// through the real davFS the route's ReadFile seam wraps.
	if _, _, err := a.davFS.Write(ctx, "admin", "/report.txt", strings.NewReader("file-body"), nil); err != nil {
		t.Fatalf("davFS write: %v", err)
	}

	send := httptest.NewRequestWithContext(ctx, http.MethodPost, "/apps/mail/api/accounts/1/send",
		strings.NewReader(`{"to":["rcpt@example.com"],"subject":"wiring","bodyPlain":"x","attachments":[{"path":"/report.txt"}]}`))
	send.Header.Set("Content-Type", "application/json")
	send.Header.Set("OCS-APIRequest", "true")
	send.SetBasicAuth("admin", "admin")
	rr = httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, send)
	if rr.Code != http.StatusOK {
		t.Fatalf("send: status = %d body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"messageId"`) {
		t.Errorf("send body = %s, want a messageId", rr.Body.String())
	}
	from, rcpts := smtpSrv.envelope()
	if !strings.Contains(from, "admin@example.com") || len(rcpts) != 1 || !strings.Contains(rcpts[0], "rcpt@example.com") {
		t.Errorf("envelope = %q / %v", from, rcpts)
	}
	data := string(smtpSrv.payload())
	if !strings.Contains(data, "Subject: wiring") {
		t.Error("DATA lacks the subject")
	}
	if !strings.Contains(data, "ZmlsZS1ib2R5") { // base64("file-body")
		t.Error("DATA lacks the base64 of the {path} attachment read through davFS")
	}
}
