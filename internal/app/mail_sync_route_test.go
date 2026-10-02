package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

// syncIMAPServer is the app-level scripted IMAP server for the M3 wiring
// tests: beyond M2's login exchange it answers LIST/EXAMINE/UID SEARCH/UID
// FETCH from a one-mailbox model so the full router can run a real sync
// end to end (egress-allowlisted loopback).
type syncIMAPServer struct {
	ln net.Listener
	wg sync.WaitGroup

	mu   sync.Mutex
	msgs []string // pre-rendered FETCH attribute bodies, one per message
}

func newSyncIMAPServer(t *testing.T) *syncIMAPServer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &syncIMAPServer{ln: ln}
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

func (f *syncIMAPServer) addr() string { return f.ln.Addr().String() }

// addMessage appends one INBOX message (uid is its position, 1-based).
func (f *syncIMAPServer) addMessage(uid int, subject, fromName, fromAddr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mb, host, _ := strings.Cut(fromAddr, "@")
	body := fmt.Sprintf("UID %d FLAGS () INTERNALDATE \"5-Jan-2006 09:00:00 +0000\" RFC822.SIZE 99 "+
		"ENVELOPE (\"5-Jan-2006 09:00:00 +0000\" \"%s\" ((\"%s\" NIL \"%s\" \"%s\")) NIL NIL ((\"Me\" NIL \"me\" \"example.com\")) NIL NIL NIL \"<m%d@example.com>\")", //nolint:dupword // ENVELOPE wire shape carries NIL runs
		uid, subject, fromName, mb, host, uid)
	f.msgs = append(f.msgs, body)
}

func (f *syncIMAPServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	_, _ = io.WriteString(conn, "* OK fake IMAP4rev1 ready\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		verb, args, _ := strings.Cut(rest, " ")
		f.mu.Lock()
		switch verb {
		case "CAPABILITY":
			_, _ = io.WriteString(conn, "* CAPABILITY IMAP4rev1\r\n"+tag+" OK capability done\r\n")
		case "LOGIN":
			_, _ = io.WriteString(conn, tag+" OK logged in\r\n")
		case "LIST":
			_, _ = io.WriteString(conn, "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n"+tag+" OK list done\r\n")
		case "EXAMINE":
			var b strings.Builder
			fmt.Fprintf(&b, "* %d EXISTS\r\n", len(f.msgs))
			b.WriteString("* OK [UIDVALIDITY 4242] UIDs valid\r\n")
			fmt.Fprintf(&b, "* OK [UIDNEXT %d] Predicted next UID\r\n", len(f.msgs)+1)
			b.WriteString(tag + " OK [READ-ONLY] examine done\r\n")
			_, _ = io.WriteString(conn, b.String())
		case "UID":
			sub, urest, _ := strings.Cut(args, " ")
			switch sub {
			case "SEARCH":
				var b strings.Builder
				b.WriteString("* SEARCH")
				for i := range f.msgs {
					fmt.Fprintf(&b, " %d", i+1)
				}
				b.WriteString("\r\n" + tag + " OK search done\r\n")
				_, _ = io.WriteString(conn, b.String())
			case "FETCH":
				set, _, _ := strings.Cut(urest, " (")
				var b strings.Builder
				seq := 0
				for i, body := range f.msgs {
					if !strings.Contains(","+set+",", ","+strconv.Itoa(i+1)+",") {
						continue
					}
					seq++
					fmt.Fprintf(&b, "* %d FETCH (%s)\r\n", seq, body)
				}
				b.WriteString(tag + " OK fetch done\r\n")
				_, _ = io.WriteString(conn, b.String())
			default:
				_, _ = io.WriteString(conn, tag+" BAD unsupported uid command\r\n")
			}
		case "LOGOUT":
			_, _ = io.WriteString(conn, "* BYE bye\r\n"+tag+" OK logout done\r\n")
			f.mu.Unlock()
			return
		default:
			_, _ = io.WriteString(conn, tag+" BAD unsupported\r\n")
		}
		f.mu.Unlock()
	}
}

// TestMailSyncWiring pins the M3 app wiring (ADR-0108 §5): with mail.enabled
// the mail.sync job is registered AND seeded before jr.Start, POST
// /apps/mail/api/accounts/{id}/sync runs a real pass through the full
// router, and a new INBOX arrival after the initial bulk sync lands a bell
// notification for the owning user.
func TestMailSyncWiring(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := newSyncIMAPServer(t)
	host, port, _ := strings.Cut(fake.addr(), ":")
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
	if a.mailSyncer == nil {
		t.Fatal("mail.enabled: mailSyncer is nil")
	}

	// mail.sync was registered before Start, so its periodic row is seeded.
	var jobRows int
	if err := a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE name = ?`, jobs.JobMailSync).Scan(&jobRows); err != nil {
		t.Fatal(err)
	}
	if jobRows != 1 {
		t.Fatalf("mail.sync seeded rows = %d, want 1", jobRows)
	}

	// Create the account against the fake, then sync twice: the first pass
	// is the initial bulk (no bell), the second carries one arrival.
	create := httptest.NewRequestWithContext(ctx, http.MethodPost, "/apps/mail/api/accounts",
		strings.NewReader(`{"emailAddress":"admin@example.com","imapHost":"`+host+`","imapPort":`+port+
			`,"imapSslMode":"none","imapUser":"u","imapPassword":"sync-wiring-pw","smtpHost":"smtp.example.com","smtpPort":25,"smtpUser":"u"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("OCS-APIRequest", "true")
	create.SetBasicAuth("admin", "admin")
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, create)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: status = %d body = %s", rr.Code, rr.Body.String())
	}

	fake.addMessage(1, "Welcome", "Alice", "alice@example.com")
	doSync := func() int {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/apps/mail/api/accounts/1/sync", nil)
		req.Header.Set("OCS-APIRequest", "true")
		req.SetBasicAuth("admin", "admin")
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("sync: status = %d body = %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			NewMessages int `json:"newMessages"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.NewMessages
	}
	if n := doSync(); n != 1 {
		t.Fatalf("initial sync new = %d, want 1", n)
	}

	admin, err := a.Users.GetByUID(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	bells, err := a.notifStore.List(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bells) != 0 {
		t.Fatalf("initial bulk sync must not ring: %+v", bells)
	}

	fake.addMessage(2, "Fresh news", "Carol", "carol@example.com")
	if n := doSync(); n != 1 {
		t.Fatalf("arrival sync new = %d, want 1", n)
	}
	bells, err = a.notifStore.List(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bells) != 1 {
		t.Fatalf("bells = %+v, want 1", bells)
	}
	bell := bells[0]
	if bell.App != "mail" || bell.UserUID != "admin" || bell.ObjectType != "mail_account" || bell.ObjectID != "1" ||
		!bell.ShouldNotify || !strings.Contains(bell.Subject, "Carol <carol@example.com>") ||
		!strings.Contains(bell.Subject, "1 new message") {
		t.Errorf("bell = %+v (subject %q)", bell, bell.Subject)
	}
}
