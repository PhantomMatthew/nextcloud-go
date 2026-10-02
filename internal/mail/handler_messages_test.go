package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// handler_messages_test.go covers the M4 message APIs (ADR-0108 §6) against
// the scripted syncFake speaking the full live-op surface, with the wire
// assertions going through its ops log.

// m4Raw1 is a multipart/alternative message (plain + html), unseen.
const m4Raw1 = "From: Carol <carol@example.com>\r\n" +
	"To: me@example.com\r\n" +
	"Subject: Hello there\r\n" +
	"Content-Type: multipart/alternative; boundary=alt\r\n" +
	"\r\n" +
	"--alt\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"plain hello\r\n" +
	"--alt\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>html hello</p>\r\n" +
	"--alt--\r\n"

// m4Raw2 is a multipart/mixed message with one base64 PDF attachment.
const m4Raw2 = "From: d@example.com\r\n" +
	"Subject: With attachment\r\n" +
	"Content-Type: multipart/mixed; boundary=mix\r\n" +
	"\r\n" +
	"--mix\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"see attached\r\n" +
	"--mix\r\n" +
	"Content-Type: application/pdf; name=\"doc.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"doc.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"UERGJXZlcnNpb24=\r\n" + // "PDF%version"
	"--mix--\r\n"

// seedM4 seeds the sync fake with an INBOX (two messages carrying
// whole-message raw bodies), a Trash, and an Archive.
func seedM4(e *mailEnv) {
	e.sfake.boxes = []*fakeMailbox{
		{
			name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 42,
			msgs: []*fakeMsg{
				{
					uid: 1, date: "2-Jan-2006 15:04:05 -0700", size: len(m4Raw1),
					subject: "Hello there", fromName: "Carol", fromAddr: "carol@example.com",
					toAddrs: []string{"me@example.com"}, msgID: "<m1@example.com>", raw: []byte(m4Raw1),
				},
				{
					uid: 2, flags: []string{"\\Seen"}, date: "3-Jan-2006 10:00:00 +0000", size: len(m4Raw2),
					subject: "With attachment", fromAddr: "d@example.com",
					toAddrs: []string{"me@example.com"}, msgID: "<m2@example.com>", raw: []byte(m4Raw2),
				},
			},
		},
		{name: "Trash", attrs: []string{"\\HasNoChildren", "\\Trash"}, delim: "/", selectable: true, uidvalidity: 43},
		{name: "Archive", attrs: []string{"\\HasNoChildren", "\\Archive"}, delim: "/", selectable: true, uidvalidity: 44},
	}
}

// m4Setup creates alice's account, seeds the fake, and runs one sync. It
// returns the account id and the {INBOX, Archive} mailbox ids.
func m4Setup(t *testing.T, e *mailEnv) (accountID, inboxID, archiveID int64) {
	t.Helper()
	accountID = createAlice(t, e)
	seedM4(e)
	base := fmt.Sprintf("%s/%d", AccountsPrefix, accountID)
	if rr := e.serve(t, "alice", http.MethodPost, base+"/sync", ""); rr.Code != http.StatusOK {
		t.Fatalf("sync: status = %d body = %s", rr.Code, rr.Body.String())
	}
	rr := e.serve(t, "alice", http.MethodGet, base+"/mailboxes", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("mailboxes: status = %d", rr.Code)
	}
	var boxes []mailboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &boxes); err != nil {
		t.Fatal(err)
	}
	var trashID int64
	for _, b := range boxes {
		switch b.Name {
		case "INBOX":
			inboxID = b.ID
		case "Trash":
			trashID = b.ID
		case "Archive":
			archiveID = b.ID
		}
	}
	if inboxID == 0 || trashID == 0 || archiveID == 0 {
		t.Fatalf("mailbox ids = %d/%d/%d (boxes %+v)", inboxID, trashID, archiveID, boxes)
	}
	return accountID, inboxID, archiveID
}

// m4ListIDs fetches the message ids of one mailbox (newest first).
func m4ListIDs(t *testing.T, e *mailEnv, accountID, mbid int64) []messageResponse {
	t.Helper()
	path := fmt.Sprintf("%s/%d/mailboxes/%d/messages", AccountsPrefix, accountID, mbid)
	rr := e.serve(t, "alice", http.MethodGet, path, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp messageListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Messages
}

// TestHandlerMessagesListPagination walks seven same-date-tied rows in
// pages of three through the cursor to exhaustion.
func TestHandlerMessagesListPagination(t *testing.T) {
	e := newMailEnv(t)
	accountID := createAlice(t, e)
	mb := &Mailbox{AccountID: accountID, Name: "INBOX", Selectable: true}
	if err := e.store.UpsertMailbox(context.Background(), mb); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{MailboxID: mb.ID, UID: 1, Subject: "s1", FromAddr: "a@x", DateUnix: 100, Flags: PackFlags([]string{"Seen"}), Size: 11},
		{MailboxID: mb.ID, UID: 2, Subject: "s2", DateUnix: 300},
		{MailboxID: mb.ID, UID: 3, Subject: "s3", DateUnix: 300},
		{MailboxID: mb.ID, UID: 4, Subject: "s4", DateUnix: 300},
		{MailboxID: mb.ID, UID: 5, Subject: "s5", DateUnix: 200},
		{MailboxID: mb.ID, UID: 6, Subject: "s6", DateUnix: 100},
		{MailboxID: mb.ID, UID: 7, Subject: "s7", DateUnix: 50},
	}
	if err := e.store.InsertMessages(context.Background(), msgs); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("%s/%d/mailboxes/%d/messages", AccountsPrefix, accountID, mb.ID)

	var got []int64
	cursor := ""
	pages := 0
	for {
		path := base + "?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rr := e.serve(t, "alice", http.MethodGet, path, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("list: status = %d body = %s", rr.Code, rr.Body.String())
		}
		var resp messageListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		pages++
		for _, m := range resp.Messages {
			got = append(got, m.UID)
		}
		if resp.NextCursor == nil {
			break
		}
		cursor = *resp.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	want := []int64{4, 3, 2, 5, 6, 1, 7} // the date-300 tie walks by id desc
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}

	// The summary shape: flags re-carry the backslash, from/date/size land.
	first := m4ListIDs(t, e, accountID, mb.ID)
	if len(first) != 7 || first[0].UID != 4 {
		t.Fatalf("full list = %+v", first)
	}
	last := first[len(first)-1]
	if last.UID != 7 || last.Flags == nil || len(last.Flags) != 0 {
		t.Errorf("flag-less row must render [], got %+v", last)
	}
	rr := e.serve(t, "alice", http.MethodGet, base+"?limit=1", "")
	var one messageListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if len(one.Messages) != 1 || one.NextCursor == nil {
		t.Fatalf("limit=1 = %+v", one)
	}
	if one.Messages[0].ID <= 0 || one.Messages[0].Date != 300 || one.Messages[0].Subject != "s4" {
		t.Errorf("summary row = %+v", one.Messages[0])
	}

	// Query validation: bad limit and bad cursor are 400s; a huge limit
	// clamps silently to 200.
	if rr := e.serve(t, "alice", http.MethodGet, base+"?limit=abc", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("limit=abc: status = %d, want 400", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, base+"?limit=0", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("limit=0: status = %d, want 400", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, base+"?cursor=garbage", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("cursor=garbage: status = %d, want 400", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, base+"?cursor=1_2_3", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("cursor=1_2_3: status = %d, want 400", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, base+"?limit=9999", ""); rr.Code != http.StatusOK {
		t.Errorf("limit=9999 must clamp, status = %d", rr.Code)
	}
	// A flag-carrying row renders the backslash form (uid 1 walks sixth).
	if len(first[5].Flags) != 1 || first[5].UID != 1 || first[5].Flags[0] != `\Seen` {
		t.Errorf("seen row must render [\"\\\\Seen\"], got %+v", first[5])
	}
}

// TestHandlerMessagesListPreviewFields: the M6 list response carries the
// sync-time preview and hasAttachments, and the detail response stays the
// M4 shape (no preview fields).
func TestHandlerMessagesListPreviewFields(t *testing.T) {
	e := newMailEnv(t)
	accountID := createAlice(t, e)
	mb := &Mailbox{AccountID: accountID, Name: "INBOX", Selectable: true}
	if err := e.store.UpsertMailbox(context.Background(), mb); err != nil {
		t.Fatal(err)
	}
	if err := e.store.InsertMessages(context.Background(), []Message{
		{MailboxID: mb.ID, UID: 1, Subject: "plain", DateUnix: 100},
		{MailboxID: mb.ID, UID: 2, Subject: "with attachment", DateUnix: 200},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetMessagePreview(context.Background(), mb.ID, 2, "see attached", true); err != nil {
		t.Fatal(err)
	}
	// The detail fetch runs live: the fake serves uid 2's raw bytes.
	e.sfake.boxes = []*fakeMailbox{
		{
			name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 7,
			msgs: []*fakeMsg{
				{
					uid: 2, date: "3-Jan-2006 10:00:00 +0000", size: len(m4Raw2), subject: "with attachment",
					fromAddr: "d@example.com", msgID: "<m2@example.com>", raw: []byte(m4Raw2),
				},
			},
		},
	}
	base := fmt.Sprintf("%s/%d/mailboxes/%d/messages", AccountsPrefix, accountID, mb.ID)
	rr := e.serve(t, "alice", http.MethodGet, base, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp messageListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("messages = %+v", resp.Messages)
	}
	if resp.Messages[0].UID != 2 || resp.Messages[0].Preview != "see attached" || !resp.Messages[0].HasAttachments {
		t.Errorf("previewed row = %+v", resp.Messages[0])
	}
	if resp.Messages[1].Preview != "" || resp.Messages[1].HasAttachments {
		t.Errorf("default row = %+v", resp.Messages[1])
	}
	// The JSON keys exist even at their zero values (the client's contract).
	body := rr.Body.String()
	if !strings.Contains(body, `"preview":""`) || !strings.Contains(body, `"hasAttachments":false`) {
		t.Errorf("zero-value keys must render: %s", body)
	}
	// The detail response does NOT grow the list fields (M4 shape pinned).
	det := e.serve(t, "alice", http.MethodGet, fmt.Sprintf("%s/%d", base, resp.Messages[0].ID), "")
	if det.Code != http.StatusOK {
		t.Fatalf("detail: status = %d body = %s", det.Code, det.Body.String())
	}
	if strings.Contains(det.Body.String(), `"preview"`) || strings.Contains(det.Body.String(), `"hasAttachments"`) {
		t.Errorf("detail must not carry the list preview fields: %s", det.Body.String())
	}
}

// TestHandlerMessagesListScopes: cross-user, cross-account-mailbox, and
// unknown paths are all 404; wrong verbs are 405.
func TestHandlerMessagesListScopes(t *testing.T) {
	e := newMailEnv(t)
	accountID, inboxID, _ := m4Setup(t, e)
	base := fmt.Sprintf("%s/%d/mailboxes/%d/messages", AccountsPrefix, accountID, inboxID)

	if rr := e.serve(t, "bob", http.MethodGet, base, ""); rr.Code != http.StatusNotFound {
		t.Errorf("bob list: status = %d, want 404", rr.Code)
	}
	// A mailbox id from somebody else's account is a 404 for alice.
	other := testAccount("bob")
	if err := e.store.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	ob := &Mailbox{AccountID: other.ID, Name: "INBOX", Selectable: true}
	if err := e.store.UpsertMailbox(context.Background(), ob); err != nil {
		t.Fatal(err)
	}
	cross := fmt.Sprintf("%s/%d/mailboxes/%d/messages", AccountsPrefix, accountID, ob.ID)
	if rr := e.serve(t, "alice", http.MethodGet, cross, ""); rr.Code != http.StatusNotFound {
		t.Errorf("cross-account mailbox: status = %d, want 404", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, fmt.Sprintf("%s/%d/mailboxes/%d/messages", AccountsPrefix, accountID, 99999), ""); rr.Code != http.StatusNotFound {
		t.Errorf("missing mailbox: status = %d, want 404", rr.Code)
	}
	// Verbs and unknown tails.
	if rr := e.serve(t, "alice", http.MethodPost, base, "{}"); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST messages: status = %d, want 405", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, fmt.Sprintf("%s/%d/mailboxes/%s/messages", AccountsPrefix, accountID, "abc"), ""); rr.Code != http.StatusNotFound {
		t.Errorf("non-numeric mbid: status = %d, want 404", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, fmt.Sprintf("%s/%d/mailboxes/%d/junk", AccountsPrefix, accountID, inboxID), ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown mailbox sub-resource: status = %d, want 404", rr.Code)
	}
}

// TestHandlerMessageDetail: bodies, attachment metadata, the default
// seen-mark (wire + local row), and markSeen=false skipping both.
func TestHandlerMessageDetail(t *testing.T) {
	t.Run("MarksSeenByDefault", func(t *testing.T) {
		e := newMailEnv(t)
		accountID, inboxID, _ := m4Setup(t, e)
		msgs := m4ListIDs(t, e, accountID, inboxID)
		if len(msgs) != 2 {
			t.Fatalf("messages = %+v", msgs)
		}
		var unseen messageResponse
		for _, m := range msgs {
			if m.UID == 1 {
				unseen = m
			}
		}
		if unseen.ID == 0 || len(unseen.Flags) != 0 {
			t.Fatalf("unseen message = %+v (messages %+v)", unseen, msgs)
		}
		detailPath := fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d", AccountsPrefix, accountID, inboxID, unseen.ID)

		// Default read: bodies land, and the message is marked \Seen — the
		// response flags, the wire, AND the local row all show it.
		rr := e.serve(t, "alice", http.MethodGet, detailPath, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("detail: status = %d body = %s", rr.Code, rr.Body.String())
		}
		var det messageDetailResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &det); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(det.BodyPlain, "plain hello") || !strings.Contains(det.BodyHTML, "html hello") {
			t.Errorf("bodies = %q / %q", det.BodyPlain, det.BodyHTML)
		}
		if !det.HTMLSanitized {
			t.Error("htmlSanitized must be true (M7 sanitizes the HTML body before responding)")
		}
		if len(det.Flags) != 1 || det.Flags[0] != `\Seen` {
			t.Errorf("flags after read = %v", det.Flags)
		}
		ops := e.sfake.opsLog()
		wantOps := []string{"SELECT INBOX", "UID FETCH 1 (UID BODY.PEEK[])", `UID STORE 1 +FLAGS.SILENT (\Seen)`}
		if fmt.Sprint(ops) != fmt.Sprint(wantOps) {
			t.Errorf("wire ops = %v, want %v", ops, wantOps)
		}
		row, err := e.store.GetMessage(context.Background(), inboxID, unseen.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Flags != " Seen " {
			t.Errorf("local row flags = %q, want ' Seen '", row.Flags)
		}
		// The fake server agrees the flag landed.
		if !hasWireFlag(e.sfake.mailbox("INBOX").msgs[0], "\\Seen") {
			t.Error("server-side \\Seen missing after the read")
		}
	})

	t.Run("MarkSeenFalse", func(t *testing.T) {
		e := newMailEnv(t)
		accountID, inboxID, _ := m4Setup(t, e)
		msgs := m4ListIDs(t, e, accountID, inboxID)
		var m1, m2 messageResponse
		for _, m := range msgs {
			switch m.UID {
			case 1:
				m1 = m
			case 2:
				m2 = m
			}
		}
		rr := e.serve(t, "alice", http.MethodGet,
			fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d?markSeen=false", AccountsPrefix, accountID, inboxID, m1.ID), "")
		if rr.Code != http.StatusOK {
			t.Fatalf("detail markSeen=false: status = %d body = %s", rr.Code, rr.Body.String())
		}
		var det messageDetailResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &det); err != nil {
			t.Fatal(err)
		}
		if len(det.Flags) != 0 {
			t.Errorf("flags = %v, want none (markSeen=false must not mark)", det.Flags)
		}
		for _, op := range e.sfake.opsLog() {
			if strings.Contains(op, "STORE") {
				t.Errorf("markSeen=false must not STORE, ops = %v", e.sfake.opsLog())
			}
		}
		row, err := e.store.GetMessage(context.Background(), inboxID, m1.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Flags != "" {
			t.Errorf("local row flags = %q, want unchanged", row.Flags)
		}

		// The attachment metadata rides the detail of the second message.
		rr = e.serve(t, "alice", http.MethodGet,
			fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d?markSeen=false", AccountsPrefix, accountID, inboxID, m2.ID), "")
		if rr.Code != http.StatusOK {
			t.Fatalf("detail m2: status = %d", rr.Code)
		}
		var det2 messageDetailResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &det2); err != nil {
			t.Fatal(err)
		}
		if len(det2.Attachments) != 1 || det2.Attachments[0].Index != 0 || det2.Attachments[0].Filename != "doc.pdf" ||
			det2.Attachments[0].ContentType != "application/pdf" || det2.Attachments[0].Size != 11 {
			t.Errorf("attachments = %+v", det2.Attachments)
		}
		if len(det2.Flags) != 1 || det2.Flags[0] != `\Seen` {
			t.Errorf("flags = %v (uid 2 was already seen)", det2.Flags)
		}
	})
}

// TestHandlerMessageDetailSanitizes: M7 — the detail endpoint runs the HTML
// body through SanitizeHTML before responding: the script, the event
// handler, and the javascript: URL are gone, the text and the safe link
// survive, and htmlSanitized is true.
func TestHandlerMessageDetailSanitizes(t *testing.T) {
	e := newMailEnv(t)
	accountID := createAlice(t, e)
	mb := &Mailbox{AccountID: accountID, Name: "INBOX", Selectable: true}
	if err := e.store.UpsertMailbox(context.Background(), mb); err != nil {
		t.Fatal(err)
	}
	raw := "From: Evil <evil@example.com>\r\n" +
		"Subject: xss\r\n" +
		"Content-Type: multipart/alternative; boundary=alt\r\n" +
		"\r\n" +
		"--alt\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"plain survives\r\n" +
		"--alt\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		`<div onclick="steal()"><script>alert(1)</script><p>safe text</p>` +
		`<a href="javascript:alert(1)">click</a>` +
		`<a href="https://example.com/">good</a></div>` +
		"\r\n--alt--\r\n"
	if err := e.store.InsertMessages(context.Background(), []Message{
		{MailboxID: mb.ID, UID: 9, Subject: "xss", FromAddr: "evil@example.com", DateUnix: 100},
	}); err != nil {
		t.Fatal(err)
	}
	e.sfake.boxes = []*fakeMailbox{
		{
			name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 7,
			msgs: []*fakeMsg{
				{
					uid: 9, date: "3-Jan-2006 10:00:00 +0000", size: len(raw), subject: "xss",
					fromAddr: "evil@example.com", msgID: "<m9@example.com>", raw: []byte(raw),
				},
			},
		},
	}
	msgs := m4ListIDs(t, e, accountID, mb.ID)
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	rr := e.serve(t, "alice", http.MethodGet,
		fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d", AccountsPrefix, accountID, mb.ID, msgs[0].ID), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("detail: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var det messageDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if !det.HTMLSanitized {
		t.Error("htmlSanitized must be true")
	}
	for _, bad := range []string{"<script", "alert(1)", "onclick", "steal()", "javascript:"} {
		if strings.Contains(det.BodyHTML, bad) {
			t.Errorf("bodyHtml must not contain %q: %s", bad, det.BodyHTML)
		}
	}
	for _, good := range []string{"safe text", "click", "good", `href="https://example.com/"`, `target="_blank"`, `rel="noopener noreferrer"`} {
		if !strings.Contains(det.BodyHTML, good) {
			t.Errorf("bodyHtml must contain %q: %s", good, det.BodyHTML)
		}
	}
	if !strings.Contains(det.BodyPlain, "plain survives") {
		t.Errorf("bodyPlain = %q", det.BodyPlain)
	}
}

// TestHandlerMessageDetailOversizedHTML: an HTML part over the sanitizer's
// 2MB guard renders bodyHtml empty (the size-guard fallback) while the
// plain body is still served.
func TestHandlerMessageDetailOversizedHTML(t *testing.T) {
	e := newMailEnv(t)
	accountID := createAlice(t, e)
	mb := &Mailbox{AccountID: accountID, Name: "INBOX", Selectable: true}
	if err := e.store.UpsertMailbox(context.Background(), mb); err != nil {
		t.Fatal(err)
	}
	raw := "From: Big <big@example.com>\r\n" +
		"Subject: huge\r\n" +
		"Content-Type: multipart/alternative; boundary=alt\r\n" +
		"\r\n" +
		"--alt\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"still here\r\n" +
		"--alt\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>" + strings.Repeat("x", sanMaxInput) + "</p>" +
		"\r\n--alt--\r\n"
	if err := e.store.InsertMessages(context.Background(), []Message{
		{MailboxID: mb.ID, UID: 10, Subject: "huge", FromAddr: "big@example.com", DateUnix: 100},
	}); err != nil {
		t.Fatal(err)
	}
	e.sfake.boxes = []*fakeMailbox{
		{
			name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 8,
			msgs: []*fakeMsg{
				{
					uid: 10, date: "3-Jan-2006 10:00:00 +0000", size: len(raw), subject: "huge",
					fromAddr: "big@example.com", msgID: "<m10@example.com>", raw: []byte(raw),
				},
			},
		},
	}
	msgs := m4ListIDs(t, e, accountID, mb.ID)
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	rr := e.serve(t, "alice", http.MethodGet,
		fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d", AccountsPrefix, accountID, mb.ID, msgs[0].ID), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("detail: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var det messageDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if det.BodyHTML != "" {
		t.Errorf("oversized bodyHtml must be empty, got %d bytes", len(det.BodyHTML))
	}
	if !strings.Contains(det.BodyPlain, "still here") {
		t.Errorf("bodyPlain = %q", det.BodyPlain)
	}
	if !det.HTMLSanitized {
		t.Error("htmlSanitized must be true even when the guard empties the HTML part")
	}
}

// TestHandlerMessageDetailGone: the row is synced but the server lost the
// message → 404.
func TestHandlerMessageDetailGone(t *testing.T) {
	e := newMailEnv(t)
	accountID, inboxID, _ := m4Setup(t, e)
	msgs := m4ListIDs(t, e, accountID, inboxID)
	e.sfake.mailbox("INBOX").msgs[0].raw = nil // the server lost uid 1's body
	rr := e.serve(t, "alice", http.MethodGet,
		fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d", AccountsPrefix, accountID, inboxID, msgs[1].ID), "")
	if msgs[1].UID != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	if rr.Code != http.StatusNotFound {
		t.Errorf("gone message: status = %d body = %s, want 404", rr.Code, rr.Body.String())
	}
}

// TestHandlerMessageFlags: add+remove over the wire, the local row, the
// updated summary, and the 400s.
func TestHandlerMessageFlags(t *testing.T) {
	e := newMailEnv(t)
	accountID, inboxID, _ := m4Setup(t, e)
	msgs := m4ListIDs(t, e, accountID, inboxID)
	var m1 messageResponse
	for _, m := range msgs {
		if m.UID == 1 {
			m1 = m
		}
	}
	flagsPath := fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d/flags", AccountsPrefix, accountID, inboxID, m1.ID)

	rr := e.serve(t, "alice", http.MethodPut, flagsPath, `{"seen":true,"answered":true,"flagged":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("flags: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var updated messageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Flags) != 2 || updated.Flags[0] != `\Seen` || updated.Flags[1] != `\Answered` {
		t.Errorf("updated flags = %v", updated.Flags)
	}
	wantOps := []string{"SELECT INBOX", `UID STORE 1 +FLAGS.SILENT (\Seen \Answered)`, `UID STORE 1 -FLAGS.SILENT (\Flagged)`}
	if ops := e.sfake.opsLog(); fmt.Sprint(ops) != fmt.Sprint(wantOps) {
		t.Errorf("wire ops = %v, want %v", ops, wantOps)
	}
	row, err := e.store.GetMessage(context.Background(), inboxID, m1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Flags != " Seen Answered " {
		t.Errorf("local row flags = %q", row.Flags)
	}
	if !hasWireFlag(e.sfake.mailbox("INBOX").msgs[0], "\\Seen") ||
		!hasWireFlag(e.sfake.mailbox("INBOX").msgs[0], "\\Answered") {
		t.Errorf("server flags = %v", e.sfake.mailbox("INBOX").msgs[0].flags)
	}

	// Remove-only is one command; removing an absent flag is harmless.
	rr = e.serve(t, "alice", http.MethodPut, flagsPath, `{"seen":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("flags remove: status = %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Flags) != 1 || updated.Flags[0] != `\Answered` {
		t.Errorf("flags after remove = %v", updated.Flags)
	}

	// Unknown flag names and empty deltas are 400; wrong verbs 405.
	if rr := e.serve(t, "alice", http.MethodPut, flagsPath, `{"seenX":true}`); rr.Code != http.StatusBadRequest {
		t.Errorf("unknown flag name: status = %d, want 400", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodPut, flagsPath, `{}`); rr.Code != http.StatusBadRequest {
		t.Errorf("empty delta: status = %d, want 400", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, flagsPath, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET flags: status = %d, want 405", rr.Code)
	}
	// Cross-user is 404.
	if rr := e.serve(t, "bob", http.MethodPut, flagsPath, `{"seen":true}`); rr.Code != http.StatusNotFound {
		t.Errorf("bob flags: status = %d, want 404", rr.Code)
	}
}

// TestHandlerMessageDeleteTrash: with a synced Trash the flow is COPY →
// STORE \Deleted → EXPUNGE, the local row is gone, and the copy lands in
// the fake's Trash.
func TestHandlerMessageDeleteTrash(t *testing.T) {
	e := newMailEnv(t)
	accountID, inboxID, _ := m4Setup(t, e)
	msgs := m4ListIDs(t, e, accountID, inboxID)
	var m2 messageResponse
	for _, m := range msgs {
		if m.UID == 2 {
			m2 = m
		}
	}
	rr := e.serve(t, "alice", http.MethodDelete,
		fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d", AccountsPrefix, accountID, inboxID, m2.ID), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: status = %d body = %s", rr.Code, rr.Body.String())
	}
	wantOps := []string{"SELECT INBOX", "UID COPY 2 Trash", `UID STORE 2 +FLAGS.SILENT (\Deleted)`, "EXPUNGE"}
	if ops := e.sfake.opsLog(); fmt.Sprint(ops) != fmt.Sprint(wantOps) {
		t.Errorf("wire ops = %v, want %v", ops, wantOps)
	}
	if _, err := e.store.GetMessage(context.Background(), inboxID, m2.ID); err == nil {
		t.Error("local row survived the delete")
	}
	if got := m4ListIDs(t, e, accountID, inboxID); len(got) != 1 {
		t.Errorf("list after delete = %+v", got)
	}
	if trashed := e.sfake.mailbox("Trash").msgs; len(trashed) != 1 || trashed[0].uid != 1 {
		t.Errorf("trash msgs = %+v (the copy must land with a fresh uid)", trashed)
	}
	if inbox := e.sfake.mailbox("INBOX").msgs; len(inbox) != 1 {
		t.Errorf("inbox after expunge = %+v", inbox)
	}
	// Deleting the already-gone row is a 404 (scope check), and cross-user
	// is 404.
	if rr := e.serve(t, "alice", http.MethodDelete,
		fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d", AccountsPrefix, accountID, inboxID, m2.ID), ""); rr.Code != http.StatusNotFound {
		t.Errorf("second delete: status = %d, want 404", rr.Code)
	}
	if rr := e.serve(t, "bob", http.MethodDelete,
		fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d", AccountsPrefix, accountID, inboxID, 1), ""); rr.Code != http.StatusNotFound {
		t.Errorf("bob delete: status = %d, want 404", rr.Code)
	}
}

// TestHandlerMessageDeleteNoTrash: without a synced trash mailbox the flow
// is STORE \Deleted → EXPUNGE only (no COPY).
func TestHandlerMessageDeleteNoTrash(t *testing.T) {
	e := newMailEnv(t)
	accountID := createAlice(t, e)
	// INBOX only — no Trash anywhere.
	e.sfake.boxes = []*fakeMailbox{
		{
			name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 42,
			msgs: []*fakeMsg{
				{uid: 1, date: "2-Jan-2006 15:04:05 -0700", size: 10, subject: "s", fromAddr: "a@x", msgID: "<a@x>", raw: []byte("Subject: s\r\n\r\nbody")},
			},
		},
	}
	base := fmt.Sprintf("%s/%d", AccountsPrefix, accountID)
	if rr := e.serve(t, "alice", http.MethodPost, base+"/sync", ""); rr.Code != http.StatusOK {
		t.Fatalf("sync: status = %d", rr.Code)
	}
	rr := e.serve(t, "alice", http.MethodGet, base+"/mailboxes", "")
	var boxes []mailboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &boxes); err != nil {
		t.Fatal(err)
	}
	msgs := m4ListIDs(t, e, accountID, boxes[0].ID)
	rr = e.serve(t, "alice", http.MethodDelete,
		fmt.Sprintf("%s/mailboxes/%d/messages/%d", base, boxes[0].ID, msgs[0].ID), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: status = %d body = %s", rr.Code, rr.Body.String())
	}
	wantOps := []string{"SELECT INBOX", `UID STORE 1 +FLAGS.SILENT (\Deleted)`, "EXPUNGE"}
	if ops := e.sfake.opsLog(); fmt.Sprint(ops) != fmt.Sprint(wantOps) {
		t.Errorf("wire ops = %v, want %v", ops, wantOps)
	}
}

// TestHandlerMessageMove: COPY to the destination + expunge, the local row
// is deleted (the next sync rediscovers the copy), and the destination
// validation holds.
func TestHandlerMessageMove(t *testing.T) {
	e := newMailEnv(t)
	accountID, inboxID, archiveID := m4Setup(t, e)
	msgs := m4ListIDs(t, e, accountID, inboxID)
	var m1 messageResponse
	for _, m := range msgs {
		if m.UID == 1 {
			m1 = m
		}
	}
	movePath := fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d/move", AccountsPrefix, accountID, inboxID, m1.ID)

	// Destination validation runs first (the successful move deletes the
	// row): missing, own mailbox, a mailbox owned by another account, and
	// a missing id are 400/404.
	if rr := e.serve(t, "alice", http.MethodPut, movePath, `{}`); rr.Code != http.StatusBadRequest {
		t.Errorf("missing dest: status = %d, want 400", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodPut, movePath, `{"destMailboxId":`+strconv.FormatInt(inboxID, 10)+`}`); rr.Code != http.StatusBadRequest {
		t.Errorf("same mailbox: status = %d, want 400", rr.Code)
	}
	other := testAccount("bob")
	if err := e.store.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	ob := &Mailbox{AccountID: other.ID, Name: "INBOX", Selectable: true}
	if err := e.store.UpsertMailbox(context.Background(), ob); err != nil {
		t.Fatal(err)
	}
	if rr := e.serve(t, "alice", http.MethodPut, movePath, `{"destMailboxId":`+strconv.FormatInt(ob.ID, 10)+`}`); rr.Code != http.StatusNotFound {
		t.Errorf("cross-account dest: status = %d, want 404", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodPut, movePath, `{"destMailboxId":99999}`); rr.Code != http.StatusNotFound {
		t.Errorf("missing dest id: status = %d, want 404", rr.Code)
	}

	rr := e.serve(t, "alice", http.MethodPut, movePath, `{"destMailboxId":`+strconv.FormatInt(archiveID, 10)+`}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("move: status = %d body = %s", rr.Code, rr.Body.String())
	}
	wantOps := []string{"SELECT INBOX", "UID COPY 1 Archive", `UID STORE 1 +FLAGS.SILENT (\Deleted)`, "EXPUNGE"}
	if ops := e.sfake.opsLog(); fmt.Sprint(ops) != fmt.Sprint(wantOps) {
		t.Errorf("wire ops = %v, want %v", ops, wantOps)
	}
	if _, err := e.store.GetMessage(context.Background(), inboxID, m1.ID); err == nil {
		t.Error("local row survived the move")
	}
	if archived := e.sfake.mailbox("Archive").msgs; len(archived) != 1 {
		t.Errorf("archive msgs = %+v", archived)
	}
	// Verbs and cross-user.
	if rr := e.serve(t, "alice", http.MethodGet, movePath, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET move: status = %d, want 405", rr.Code)
	}
	if rr := e.serve(t, "bob", http.MethodPut, movePath, `{"destMailboxId":2}`); rr.Code != http.StatusNotFound {
		t.Errorf("bob move: status = %d, want 404", rr.Code)
	}
}

// TestHandlerMessageAttachment: the download lands bytes + headers; a bad
// index and cross-user are 404.
func TestHandlerMessageAttachment(t *testing.T) {
	e := newMailEnv(t)
	accountID, inboxID, _ := m4Setup(t, e)
	msgs := m4ListIDs(t, e, accountID, inboxID)
	var m2 messageResponse
	for _, m := range msgs {
		if m.UID == 2 {
			m2 = m
		}
	}
	attPath := fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d/attachments", AccountsPrefix, accountID, inboxID, m2.ID)
	rr := e.serve(t, "alice", http.MethodGet, attPath+"/0", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("attachment: status = %d body = %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "doc.pdf") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "11" {
		t.Errorf("Content-Length = %q", cl)
	}
	if rr.Body.String() != "PDF%version" {
		t.Errorf("bytes = %q", rr.Body.String())
	}
	// The attachment download fetches but never marks seen.
	for _, op := range e.sfake.opsLog() {
		if strings.Contains(op, "STORE") {
			t.Errorf("attachment download must not STORE, ops = %v", e.sfake.opsLog())
		}
	}

	if rr := e.serve(t, "alice", http.MethodGet, attPath+"/9", ""); rr.Code != http.StatusNotFound {
		t.Errorf("bad index: status = %d, want 404", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, attPath+"/-1", ""); rr.Code != http.StatusNotFound {
		t.Errorf("negative index: status = %d, want 404", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, attPath+"/x", ""); rr.Code != http.StatusNotFound {
		t.Errorf("non-numeric index: status = %d, want 404", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodPost, attPath+"/0", ""); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST attachment: status = %d, want 405", rr.Code)
	}
	if rr := e.serve(t, "bob", http.MethodGet, attPath+"/0", ""); rr.Code != http.StatusNotFound {
		t.Errorf("bob attachment: status = %d, want 404", rr.Code)
	}
}

// TestHandlerMessageOpsUpstream: a dead IMAP server maps dial failures to
// 502 (the account exists and the request was valid).
func TestHandlerMessageOpsUpstream(t *testing.T) {
	e := newMailEnv(t)
	accountID, inboxID, _ := m4Setup(t, e)
	msgs := m4ListIDs(t, e, accountID, inboxID)
	// Point the ops dialer at a dead server.
	e.ops.DialIMAP = func(context.Context, imap.DialOptions) (*imap.Client, error) {
		return nil, errors.New("connection refused")
	}
	rr := e.serve(t, "alice", http.MethodGet,
		fmt.Sprintf("%s/%d/mailboxes/%d/messages/%d", AccountsPrefix, accountID, inboxID, msgs[0].ID), "")
	if rr.Code != http.StatusBadGateway {
		t.Errorf("dead server: status = %d body = %s, want 502", rr.Code, rr.Body.String())
	}
}
