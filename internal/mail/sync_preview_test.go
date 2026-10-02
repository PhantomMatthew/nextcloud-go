package mail

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// sync_preview_test.go covers the M6 insert-time list previews (ADR-0108):
// the newest syncPreviewWindow newly synced messages up to
// syncPreviewMaxSize are fetched whole over the sync's own connection, and
// their rows gain the whitespace-collapsed plain-body snippet and the
// has-attachments flag. Failures skip their message and never fail the
// sync.

// m6RawPlain is a single-part text/plain message whose body carries
// whitespace runs and newlines (the preview collapses them).
const m6RawPlain = "From: Ann <ann@example.com>\r\n" +
	"To: me@example.com\r\n" +
	"Subject: Plain\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Hello   there,\n  this   is\r\n\ta   spaced   out\n\nbody.\r\n"

// m6RawHTMLOnly is a multipart/alternative with no plain leaf: the preview
// stays empty (HTML is never stripped into one).
const m6RawHTMLOnly = "From: h@example.com\r\n" +
	"Subject: HTML only\r\n" +
	"Content-Type: multipart/alternative; boundary=alt\r\n" +
	"\r\n" +
	"--alt\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>markup only</p>\r\n" +
	"--alt--\r\n"

// seedPreview seeds one INBOX whose messages all carry raw bodies: uid 1 a
// whitespace-messy plain body, uid 2 the M4 attachment message, uid 3 an
// HTML-only multipart.
func (e *syncEnv) seedPreview() {
	e.fake.boxes = []*fakeMailbox{
		{
			name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 3003,
			msgs: []*fakeMsg{
				{
					uid: 1, date: "2-Jan-2006 15:04:05 +0000", size: len(m6RawPlain),
					subject: "Plain", fromAddr: "ann@example.com", msgID: "<p1@example.com>", raw: []byte(m6RawPlain),
				},
				{
					uid: 2, date: "3-Jan-2006 10:00:00 +0000", size: len(m4Raw2),
					subject: "With attachment", fromAddr: "d@example.com", msgID: "<p2@example.com>", raw: []byte(m4Raw2),
				},
				{
					uid: 3, date: "4-Jan-2006 08:30:00 +0000", size: len(m6RawHTMLOnly),
					subject: "HTML only", fromAddr: "h@example.com", msgID: "<p3@example.com>", raw: []byte(m6RawHTMLOnly),
				},
			},
		},
	}
}

func (e *syncEnv) inboxID(t *testing.T) int64 {
	t.Helper()
	boxes, err := e.store.ListMailboxes(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 {
		t.Fatalf("mailboxes = %+v", boxes)
	}
	return boxes[0].ID
}

func TestSyncPreviewFillsListView(t *testing.T) {
	e := newSyncEnv(t)
	e.seedPreview()
	if n := e.sync(t); n != 3 {
		t.Fatalf("new = %d, want 3", n)
	}
	rows := e.messageRows(t, e.inboxID(t))
	if rows[0].Preview != "Hello there, this is a spaced out body." || rows[0].HasAttachments {
		t.Errorf("row 1 = %+v", rows[0])
	}
	if rows[1].Preview != "see attached" || !rows[1].HasAttachments {
		t.Errorf("row 2 (attachment) = %+v", rows[1])
	}
	if rows[2].Preview != "" || rows[2].HasAttachments {
		t.Errorf("row 3 (html-only) = %+v", rows[2])
	}
	// The previews rode the sync's EXAMINE session: every fetch is recorded,
	// none joined the live-op log.
	fetches := e.fake.fullFetchLog()
	if fmt.Sprint(fetches) != fmt.Sprint([]string{"3", "2", "1"}) {
		t.Errorf("full fetches = %v, want newest-first per-message fetches", fetches)
	}
	if ops := e.fake.opsLog(); len(ops) != 0 {
		t.Errorf("sync previews must not join the live-op log, ops = %v", ops)
	}
}

func TestSyncPreviewSkipsOversized(t *testing.T) {
	e := newSyncEnv(t)
	e.seedPreview()
	// uid 2's synced size crosses the 1 MiB preview cap: it is never fetched.
	e.fake.mailbox("INBOX").msgs[1].size = syncPreviewMaxSize + 1
	if n := e.sync(t); n != 3 {
		t.Fatalf("new = %d, want 3", n)
	}
	rows := e.messageRows(t, e.inboxID(t))
	if rows[1].Preview != "" || rows[1].HasAttachments {
		t.Errorf("oversized row = %+v, want the empty defaults", rows[1])
	}
	if fetches := e.fake.fullFetchLog(); fmt.Sprint(fetches) != fmt.Sprint([]string{"3", "1"}) {
		t.Errorf("full fetches = %v, want the oversized message skipped", fetches)
	}
	// The other two previews landed normally.
	if rows[0].Preview == "" || rows[2].HasAttachments {
		t.Errorf("rows = %+v", rows)
	}
}

func TestSyncPreviewGoneMessageSkipped(t *testing.T) {
	e := newSyncEnv(t)
	e.seedPreview()
	// The server lost uid 2's body between the summary and the full fetch.
	e.fake.mailbox("INBOX").msgs[1].raw = nil
	if n := e.sync(t); n != 3 {
		t.Fatalf("new = %d, want 3", n)
	}
	rows := e.messageRows(t, e.inboxID(t))
	if rows[1].Preview != "" || rows[1].HasAttachments {
		t.Errorf("gone row = %+v, want the empty defaults", rows[1])
	}
	if rows[0].Preview == "" {
		t.Errorf("row 1 preview lost alongside the gone message: %+v", rows)
	}
}

func TestSyncPreviewFetchFailureNeverFailsSync(t *testing.T) {
	e := newSyncEnv(t)
	e.seedPreview()
	e.fake.setFailFull(true)
	// UID FETCH BODY.PEEK[] answers NO: every preview stays empty, the sync
	// result stands (cursor advanced, flags refreshed).
	if n := e.sync(t); n != 3 {
		t.Fatalf("new = %d, want 3 (previews must not fail the sync)", n)
	}
	rows := e.messageRows(t, e.inboxID(t))
	for i := range rows {
		if rows[i].Preview != "" || rows[i].HasAttachments {
			t.Errorf("row %d = %+v, want the empty defaults", i, rows[i])
		}
	}
	if fetches := e.fake.fullFetchLog(); len(fetches) == 0 {
		t.Error("the preview fetch was attempted and refused")
	}
	counts, err := e.store.ListMailboxCounts(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Total != 3 || counts[0].LastSeenUID != 3 {
		t.Errorf("counts = %+v (the sync state must advance)", counts[0])
	}
}

func TestSyncPreviewWindowNewestFifty(t *testing.T) {
	e := newSyncEnv(t)
	// 60 new messages: only the newest 50 (uids 11-60) get previews.
	big := &fakeMailbox{name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 4004}
	for i := 1; i <= 60; i++ {
		raw := fmt.Sprintf("Subject: m%d\r\nContent-Type: text/plain\r\n\r\nbody of message %d\r\n", i, i)
		big.msgs = append(big.msgs, &fakeMsg{
			uid: int64(i), date: "2-Jan-2006 15:04:05 +0000", size: len(raw),
			subject: fmt.Sprintf("m%d", i), fromAddr: "a@example.com", msgID: fmt.Sprintf("<w%d@x>", i), raw: []byte(raw),
		})
	}
	e.fake.boxes = []*fakeMailbox{big}
	if n := e.sync(t); n != 60 {
		t.Fatalf("new = %d, want 60", n)
	}
	rows := e.messageRows(t, e.inboxID(t))
	for i := range rows {
		want := ""
		if rows[i].UID > 10 {
			want = fmt.Sprintf("body of message %d", rows[i].UID)
		}
		if rows[i].Preview != want {
			t.Errorf("uid %d preview = %q, want %q", rows[i].UID, rows[i].Preview, want)
		}
	}
	if fetches := e.fake.fullFetchLog(); len(fetches) != syncPreviewWindow {
		t.Errorf("full fetches = %d, want %d (the newest-50 window)", len(fetches), syncPreviewWindow)
	}
}

func TestSyncPreviewNoRefetchOnSecondRun(t *testing.T) {
	e := newSyncEnv(t)
	e.seedPreview()
	e.sync(t)
	if n := e.sync(t); n != 0 {
		t.Fatalf("second sync new = %d, want 0", n)
	}
	// Previews are an insert-time extra: a no-op run fetches nothing.
	if fetches := e.fake.fullFetchLog(); len(fetches) != 3 {
		t.Errorf("full fetches after a no-op run = %v, want the first run's three", fetches)
	}
}

func TestPreviewText(t *testing.T) {
	t.Parallel()
	if got := previewText("  a \n\t b\r\n c  "); got != "a b c" {
		t.Errorf("collapse = %q", got)
	}
	if got := previewText(""); got != "" {
		t.Errorf("empty = %q", got)
	}
	long := previewText(strings.Repeat("x", 300))
	if runes := len([]rune(long)); runes != previewRunes {
		t.Errorf("cap = %d runes, want %d", runes, previewRunes)
	}
	// The cap counts runes, not bytes: 300 CJK runes clip to 200.
	cjk := previewText(strings.Repeat("界", 300))
	if runes := len([]rune(cjk)); runes != previewRunes {
		t.Errorf("cjk cap = %d runes, want %d", runes, previewRunes)
	}
}
