package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

// mailFixture seeds the mail store: alice with two accounts (one carrying
// INBOX + Archive, one carrying INBOX) and bob with one account — the
// provider must join across a caller's accounts and mailboxes and never
// leak another user's rows.
type mailFixture struct {
	store       *mail.SQLStore
	aliceAcct1  *mail.Account
	aliceInbox  *mail.Mailbox
	aliceSent   *mail.Mailbox
	aliceAcct2  *mail.Account
	aliceInbox2 *mail.Mailbox
}

func newMailFixture(t *testing.T) *mailFixture {
	t.Helper()
	ctx := context.Background()
	store := mail.NewSQLStore(testDB(t))
	f := &mailFixture{store: store}
	mkAccount := func(uid, email string) *mail.Account {
		a := &mail.Account{
			UserID: uid, Name: email, Email: email,
			IMAPHost: "imap.example.com", IMAPPort: 993, IMAPUser: email,
			SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPUser: email,
			PasswordSealed: []byte("sealed"),
		}
		if err := store.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	mkMailbox := func(a *mail.Account, name string) *mail.Mailbox {
		mb := &mail.Mailbox{AccountID: a.ID, Name: name, Selectable: true}
		if err := store.UpsertMailbox(ctx, mb); err != nil {
			t.Fatal(err)
		}
		return mb
	}
	f.aliceAcct1 = mkAccount("alice", "alice@example.com")
	f.aliceInbox = mkMailbox(f.aliceAcct1, "INBOX")
	f.aliceSent = mkMailbox(f.aliceAcct1, "Sent")
	f.aliceAcct2 = mkAccount("alice", "alice@work.example.com")
	f.aliceInbox2 = mkMailbox(f.aliceAcct2, "INBOX")
	bobAcct := mkAccount("bob", "bob@example.com")
	bobInbox := mkMailbox(bobAcct, "INBOX")
	msgs := []mail.Message{
		{MailboxID: f.aliceInbox.ID, UID: 1, Subject: "Quarterly report", FromAddr: "Boss <boss@example.com>", DateUnix: 100},
		{MailboxID: f.aliceSent.ID, UID: 2, Subject: "re: sync", FromAddr: "reports-bot@example.com", DateUnix: 300},
		{MailboxID: f.aliceInbox2.ID, UID: 3, Subject: "report archive", FromAddr: "noreply@example.com", DateUnix: 200},
		{MailboxID: f.aliceInbox.ID, UID: 4, Subject: "Nothing alike", FromAddr: "x@example.com", DateUnix: 400},
		{MailboxID: bobInbox.ID, UID: 5, Subject: "report for bob", FromAddr: "boss@example.com", DateUnix: 500},
	}
	if err := store.InsertMessages(ctx, msgs); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMailProviderSearch(t *testing.T) {
	f := newMailFixture(t)
	p := NewMailProvider(f.store)
	if p.ID() != "mail" || p.Name() != "Mail" {
		t.Fatalf("identity = %q/%q", p.ID(), p.Name())
	}
	ctx := context.Background()

	// Subject and sender matches join across both of alice's accounts,
	// newest first.
	hits, err := p.Search(ctx, "alice", "report", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Title != "re: sync" || hits[0].Subline != "reports-bot@example.com" {
		t.Errorf("hit 0 (from match) = %+v", hits[0])
	}
	wantURL0 := fmt.Sprintf("/apps/mail/api/accounts/%d/mailboxes/%d/messages/", f.aliceAcct1.ID, f.aliceSent.ID)
	if !strings.HasPrefix(hits[0].ResourceURL, wantURL0) {
		t.Errorf("hit 0 link = %q, want prefix %q", hits[0].ResourceURL, wantURL0)
	}
	if hits[1].Title != "report archive" {
		t.Errorf("hit 1 (second account) = %+v", hits[1])
	}
	wantURL1 := fmt.Sprintf("/apps/mail/api/accounts/%d/mailboxes/%d/messages/", f.aliceAcct2.ID, f.aliceInbox2.ID)
	if !strings.HasPrefix(hits[1].ResourceURL, wantURL1) {
		t.Errorf("hit 1 link = %q, want prefix %q", hits[1].ResourceURL, wantURL1)
	}
	if hits[2].Title != "Quarterly report" || hits[2].Subline != "Boss <boss@example.com>" {
		t.Errorf("hit 2 (subject match) = %+v", hits[2])
	}

	// User isolation: bob's own match only; a no-match term is empty; the
	// limit passes through to the store.
	if hits, err := p.Search(ctx, "bob", "report", 20); err != nil || len(hits) != 1 || hits[0].Title != "report for bob" {
		t.Errorf("bob hits = %v %v", hits, err)
	}
	if hits, err := p.Search(ctx, "alice", "no-such-term", 20); err != nil || len(hits) != 0 {
		t.Errorf("no-match = %v %v", hits, err)
	}
	if hits, err := p.Search(ctx, "alice", "  ", 20); err != nil || len(hits) != 0 {
		t.Errorf("blank term = %v %v", hits, err)
	}
	if hits, err := p.Search(ctx, "alice", "report", 1); err != nil || len(hits) != 1 || hits[0].Title != "re: sync" {
		t.Errorf("limit 1 = %v %v", hits, err)
	}

	// A nil store yields no hits (the provider is registered only when the
	// Mail app is enabled, so this is the defensive path).
	var nilP *MailProvider
	if hits, err := nilP.Search(ctx, "alice", "report", 20); err != nil || hits != nil {
		t.Errorf("nil provider = %v %v", hits, err)
	}
	if hits, err := NewMailProvider(nil).Search(ctx, "alice", "report", 20); err != nil || hits != nil {
		t.Errorf("nil store = %v %v", hits, err)
	}
}

// TestMailProviderThroughHandler pins the OCS entry shape of a mail hit:
// the mail provider lists alongside files, and its entries carry the
// message's API link under the request base.
func TestMailProviderThroughHandler(t *testing.T) {
	f := newMailFixture(t)
	h := Handler{
		Providers: []Provider{NewMailProvider(f.store)},
		Version:   ocs.V2,
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ocs/v2.php/search/providers?format=json", nil)
	h.ServeHTTP(rr, withUser(req, "alice"))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"id":"mail"`) || !strings.Contains(rr.Body.String(), `"name":"Mail"`) {
		t.Fatalf("list: status = %d body = %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/search/providers/mail/search?format=json&term=report", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req, "alice"))
	if rr.Code != http.StatusOK {
		t.Fatalf("search: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data, _ := env["ocs"].(map[string]any)["data"].(map[string]any)
	if data["name"] != "Mail" {
		t.Fatalf("data name = %v", data["name"])
	}
	entries, _ := data["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("entries = %v", entries)
	}
	entry, _ := entries[0].(map[string]any)
	wantURL := fmt.Sprintf("https://cloud.example.com/apps/mail/api/accounts/%d/mailboxes/%d/messages/",
		f.aliceAcct1.ID, f.aliceSent.ID)
	if entry["title"] != "re: sync" || entry["subline"] != "reports-bot@example.com" ||
		!strings.HasPrefix(entry["resourceUrl"].(string), wantURL) {
		t.Errorf("entry = %#v, want url prefix %q", entry, wantURL)
	}
	if entry["thumbnailUrl"] != "" || entry["icon"] != "" || entry["rounded"] != false {
		t.Errorf("entry decoration = %#v", entry)
	}

	// Bob searching the same term cannot see alice's messages.
	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/search/providers/mail/search?format=json&term=report", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req, "bob"))
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data, _ = env["ocs"].(map[string]any)["data"].(map[string]any)
	entries, _ = data["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("bob entries = %v", entries)
	}
	entry, _ = entries[0].(map[string]any)
	if entry["title"] != "report for bob" {
		t.Errorf("bob entry = %#v", entry)
	}
}
