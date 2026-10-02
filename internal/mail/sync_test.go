package mail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// syncEnv wires a real SQLStore (migration chain through 0028), the
// scripted syncFake IMAP server, and a real Syncer with an events.Bus whose
// mail.message.arrived events the tests capture.
type syncEnv struct {
	store  *SQLStore
	fake   *syncFake
	syncer *Syncer
	acct   *Account

	mu      sync.Mutex
	arrived []Arrival
}

func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)
	fake := newSyncFake(t)
	bus := events.NewBus(nil)
	e := &syncEnv{store: store, fake: fake}
	e.syncer = &Syncer{Store: store, Secret: "test-instance-secret", DialIMAP: fake.dialIMAP, Bus: bus}
	bus.Subscribe(func(_ context.Context, ev events.Event) {
		if ev.Topic != EventMessageArrived {
			return
		}
		a, err := DecodeArrival(ev.Payload)
		if err != nil {
			t.Errorf("decode arrival: %v", err)
			return
		}
		e.mu.Lock()
		e.arrived = append(e.arrived, a)
		e.mu.Unlock()
	})

	packed, err := joinPasswords("imap-pw", "imap-pw")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealCredential("test-instance-secret", "alice", "imap.example.com", "alice@example.com", packed)
	if err != nil {
		t.Fatal(err)
	}
	acct := testAccount("alice")
	acct.PasswordSealed = sealed
	if err := store.Create(ctx, acct); err != nil {
		t.Fatal(err)
	}
	e.acct = acct
	return e
}

func (e *syncEnv) arrivals() []Arrival {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Arrival(nil), e.arrived...)
}

func (e *syncEnv) sync(t *testing.T) int {
	t.Helper()
	n, err := e.syncer.SyncAccount(context.Background(), e.acct)
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	return n
}

// seedAccount gives the fake a standard two-mailbox model: an INBOX with
// three messages and an empty Sent.
func (e *syncEnv) seedAccount() {
	e.fake.boxes = []*fakeMailbox{
		{
			name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 1001,
			msgs: []*fakeMsg{
				{
					uid: 1, flags: []string{"\\Seen"}, date: "2-Jan-2006 15:04:05 -0700", size: 101,
					subject: "Hello", fromName: "Alice", fromAddr: "alice@example.com", toAddrs: []string{"me@example.com"}, msgID: "<m1@example.com>",
				},
				{
					uid: 2, flags: nil, date: "3-Jan-2006 10:00:00 +0000", size: 202,
					subject: "=?UTF-8?B?5pel5pys6Kqe?=", fromName: "=?UTF-8?Q?J=C3=BCrgen?=", fromAddr: "j@example.de", msgID: "<m2@example.de>",
				},
				{
					uid: 3, flags: []string{"\\Seen", "\\Flagged"}, date: "4-Jan-2006 08:30:00 +0000", size: 303,
					subject: "Weekly", fromAddr: "bot@example.com", toNames: []string{"Me", "Other"}, toAddrs: []string{"me@example.com", "o@example.com"}, msgID: "<m3@example.com>",
				},
			},
		},
		{name: "Sent", attrs: []string{"\\HasNoChildren", "\\Sent"}, delim: "/", selectable: true, uidvalidity: 2002},
	}
}

func TestSyncInitialPopulates(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()
	if n := e.sync(t); n != 3 {
		t.Fatalf("new = %d, want 3", n)
	}

	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 {
		t.Fatalf("mailboxes = %+v", counts)
	}
	inbox, sent := counts[0], counts[1]
	if inbox.Name != "INBOX" || inbox.Total != 3 || inbox.Unread != 1 ||
		inbox.UIDValidity != 1001 || inbox.UIDNext != 4 || inbox.LastSeenUID != 3 {
		t.Errorf("inbox = %+v", inbox)
	}
	if sent.Name != "Sent" || sent.SpecialUse != "sent" || sent.Total != 0 {
		t.Errorf("sent = %+v", sent)
	}

	// Summary fields land decoded and formatted: encoded-word subject and
	// from-name decoded, message-id stripped of <>, tos comma-joined.
	uids, err := e.store.ListMessageUIDs(context.Background(), inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uids, []int64{1, 2, 3}) {
		t.Fatalf("uids = %v", uids)
	}
	rows := e.messageRows(t, inbox.ID)
	if rows[0].Subject != "Hello" || rows[0].FromAddr != "Alice <alice@example.com>" ||
		rows[0].MessageID != "m1@example.com" || rows[0].Flags != " Seen " || rows[0].Size != 101 {
		t.Errorf("row 1 = %+v", rows[0])
	}
	if rows[1].Subject != "日本語" || rows[1].FromAddr != "Jürgen <j@example.de>" || rows[1].Flags != "" {
		t.Errorf("row 2 (encoded-word) = %+v", rows[1])
	}
	if rows[2].ToAddrs != "Me <me@example.com>, Other <o@example.com>" || rows[2].Flags != " Seen Flagged " {
		t.Errorf("row 3 = %+v", rows[2])
	}
	wantDate := time.Date(2006, 1, 2, 15, 4, 5, 0, time.FixedZone("", -7*3600)).Unix()
	if rows[0].DateUnix != wantDate {
		t.Errorf("row 1 date = %d, want %d", rows[0].DateUnix, wantDate)
	}

	// The initial bulk sync publishes no arrival events.
	if got := e.arrivals(); len(got) != 0 {
		t.Errorf("initial sync published %d events", len(got))
	}
}

func TestSyncSecondRunNoop(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()
	e.sync(t)
	if n := e.sync(t); n != 0 {
		t.Fatalf("second sync new = %d, want 0", n)
	}
	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Total != 3 || counts[0].Unread != 1 {
		t.Errorf("counts after second sync = %+v", counts[0])
	}
	if got := e.arrivals(); len(got) != 0 {
		t.Errorf("no-op sync published %d events", len(got))
	}
}

func TestSyncNewArrivalPublishesEvent(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()
	e.sync(t)

	inbox := e.fake.mailbox("INBOX")
	inbox.msgs = append(inbox.msgs, &fakeMsg{
		uid: 4, date: "5-Jan-2006 09:00:00 +0000", size: 404,
		subject: "Fresh", fromName: "Carol", fromAddr: "carol@example.com", msgID: "<m4@example.com>",
	})
	// A new message in a NON-INBOX mailbox must not publish.
	sent := e.fake.mailbox("Sent")
	sent.msgs = append(sent.msgs, &fakeMsg{
		uid: 50, date: "5-Jan-2006 09:05:00 +0000", size: 55,
		subject: "Mine", fromAddr: "me@example.com", msgID: "<m5@example.com>",
	})

	if n := e.sync(t); n != 2 {
		t.Fatalf("new = %d, want 2", n)
	}
	got := e.arrivals()
	if len(got) != 1 {
		t.Fatalf("arrivals = %+v", got)
	}
	a := got[0]
	if a.UserID != "alice" || a.AccountID != e.acct.ID || a.Count != 1 ||
		a.LatestFrom != "Carol <carol@example.com>" || a.LatestSubject != "Fresh" {
		t.Errorf("arrival = %+v", a)
	}
	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.MailboxID != counts[0].ID {
		t.Errorf("arrival mailbox id = %d, want %d", a.MailboxID, counts[0].ID)
	}
	if counts[0].Total != 4 || counts[0].Unread != 2 || counts[1].Total != 1 {
		t.Errorf("counts = %+v", counts)
	}
}

func TestSyncUIDValidityChangeWipes(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()
	e.sync(t)

	inbox := e.fake.mailbox("INBOX")
	inbox.uidvalidity = 9090 // the server's uid space was reborn
	inbox.msgs = []*fakeMsg{
		{uid: 1, date: "6-Jan-2006 00:00:00 +0000", size: 1, subject: "Reborn", fromAddr: "x@example.com", msgID: "<r1@example.com>"},
	}
	if n := e.sync(t); n != 1 {
		t.Fatalf("new = %d, want 1", n)
	}
	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Total != 1 || counts[0].UIDValidity != 9090 || counts[0].LastSeenUID != 1 {
		t.Errorf("counts after wipe = %+v", counts[0])
	}
	rows := e.messageRows(t, counts[0].ID)
	if rows[0].Subject != "Reborn" {
		t.Errorf("stale rows survived the wipe: %+v", rows)
	}
	// A UIDVALIDITY re-sync is not an arrival.
	if got := e.arrivals(); len(got) != 0 {
		t.Errorf("uidvalidity wipe published %d events", len(got))
	}
}

func TestSyncVanishedUIDDeleted(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()
	e.sync(t)

	inbox := e.fake.mailbox("INBOX")
	inbox.msgs = inbox.msgs[:2] // uid 3 vanished from the server
	if n := e.sync(t); n != 0 {
		t.Fatalf("new = %d", n)
	}
	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Total != 2 {
		t.Errorf("counts after vanish = %+v", counts[0])
	}
	uids, err := e.store.ListMessageUIDs(context.Background(), counts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uids, []int64{1, 2}) {
		t.Errorf("uids = %v", uids)
	}
}

func TestSyncFlagRefresh(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()
	e.sync(t)

	inbox := e.fake.mailbox("INBOX")
	inbox.msgs[1].flags = []string{"\\Seen"} // uid 2 got read elsewhere
	if n := e.sync(t); n != 0 {
		t.Fatalf("new = %d", n)
	}
	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Unread != 0 {
		t.Errorf("unread after flag refresh = %d", counts[0].Unread)
	}
	rows := e.messageRows(t, counts[0].ID)
	if rows[1].Flags != " Seen " {
		t.Errorf("row 2 flags = %q", rows[1].Flags)
	}
}

func TestSyncMailboxVanished(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()
	e.sync(t)
	// Sent disappears from the server entirely.
	e.fake.boxes = e.fake.boxes[:1]
	e.sync(t)
	mbs, err := e.store.ListMailboxes(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mbs) != 1 || mbs[0].Name != "INBOX" {
		t.Errorf("mailboxes = %+v", mbs)
	}
}

func TestSyncPerMailboxFailureIsolation(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()
	e.fake.mailbox("Sent").fail = true
	// Sent fails EXAMINE; INBOX must still sync, and the run succeeds.
	if n := e.sync(t); n != 3 {
		t.Fatalf("new = %d, want 3", n)
	}
	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Total != 3 {
		t.Errorf("inbox counts = %+v", counts[0])
	}
	if counts[1].LastSeenUID != 0 || counts[1].UIDValidity != 0 {
		t.Errorf("failed mailbox must stay untouched: %+v", counts[1])
	}
}

func TestSyncAccountLevelFailures(t *testing.T) {
	e := newSyncEnv(t)
	e.seedAccount()

	// LIST fails → the account sync errors before touching anything.
	e.fake.failList = true
	if _, err := e.syncer.SyncAccount(context.Background(), e.acct); err == nil {
		t.Fatal("sync with a broken LIST succeeded")
	}
	e.fake.failList = false

	// An unreachable server is an account-level error.
	dead := &Syncer{
		Store: e.store, Secret: "test-instance-secret",
		DialIMAP: func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error) {
			opts.SSLMode = SSLModeNone
			opts.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:1")
			}
			return imap.Dial(ctx, opts)
		},
	}
	if _, err := dead.SyncAccount(context.Background(), e.acct); err == nil {
		t.Fatal("sync against a dead server succeeded")
	}

	// A credential blob that does not open is an account-level error.
	bad := *e.acct
	bad.PasswordSealed = []byte("not-a-valid-blob")
	if _, err := e.syncer.SyncAccount(context.Background(), &bad); !errors.Is(err, ErrCredential) {
		t.Fatalf("bad blob: err = %v", err)
	}
}

func TestSyncFetchBatching(t *testing.T) {
	e := newSyncEnv(t)
	// 600 messages cross the 500-per-UID-FETCH batch boundary.
	big := &fakeMailbox{name: "Big", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 7}
	for i := 1; i <= 600; i++ {
		big.msgs = append(big.msgs, &fakeMsg{
			uid: int64(i), date: "2-Jan-2006 15:04:05 -0700", size: i,
			subject: fmt.Sprintf("m%d", i), fromAddr: "a@example.com", msgID: fmt.Sprintf("<m%d@x>", i),
		})
	}
	e.fake.boxes = []*fakeMailbox{big}
	if n := e.sync(t); n != 600 {
		t.Fatalf("new = %d, want 600", n)
	}
	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Total != 600 || counts[0].LastSeenUID != 600 {
		t.Errorf("counts = %+v", counts[0])
	}
}

func TestDiffUIDs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		local, server   []int64
		vanished, fresh []int64
	}{
		{nil, nil, nil, nil},
		{[]int64{1, 2, 3}, []int64{1, 2, 3}, nil, nil},
		{[]int64{1, 2, 3}, []int64{1, 3, 4, 5}, []int64{2}, []int64{4, 5}},
		{nil, []int64{9, 2, 7}, nil, []int64{2, 7, 9}}, // fresh sorted ascending
		{[]int64{5, 6}, nil, []int64{5, 6}, nil},
	}
	for _, tc := range cases {
		vanished, fresh := diffUIDs(tc.local, tc.server)
		if !reflect.DeepEqual(vanished, tc.vanished) || !reflect.DeepEqual(fresh, tc.fresh) {
			t.Errorf("diffUIDs(%v, %v) = %v, %v; want %v, %v", tc.local, tc.server, vanished, fresh, tc.vanished, tc.fresh)
		}
	}
}

// messageRows reads one mailbox's rows back in uid order for field asserts.
func (e *syncEnv) messageRows(t *testing.T, mailboxID int64) []Message {
	t.Helper()
	uids, err := e.store.ListMessageUIDs(context.Background(), mailboxID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]Message, 0, len(uids))
	for _, uid := range uids {
		var m Message
		err := e.store.db.QueryRow(context.Background(), `
SELECT mailbox_id, uid, message_id, subject, from_addr, to_addrs, date_unix, flags, size, preview, has_attachments
FROM mail_messages WHERE mailbox_id = ? AND uid = ?`, mailboxID, uid).
			Scan(&m.MailboxID, &m.UID, &m.MessageID, &m.Subject, &m.FromAddr, &m.ToAddrs, &m.DateUnix, &m.Flags, &m.Size,
				&m.Preview, &m.HasAttachments)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}
