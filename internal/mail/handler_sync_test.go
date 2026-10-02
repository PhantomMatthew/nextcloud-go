package mail

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// handler_sync_test.go covers the M3 endpoints: GET .../mailboxes, POST
// .../sync, and the synced mailbox list on GET-one-account.

// seedSyncFake gives the env's sync fake an INBOX (one seen, one unseen)
// and a CJK-wire-named mailbox with one unseen message.
func seedSyncFake(e *mailEnv) {
	e.sfake.boxes = []*fakeMailbox{
		{
			name: "INBOX", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 42,
			msgs: []*fakeMsg{
				{
					uid: 1, flags: []string{"\\Seen"}, date: "2-Jan-2006 15:04:05 -0700", size: 11,
					subject: "Read one", fromAddr: "a@example.com", msgID: "<a@x>",
				},
				{
					uid: 2, date: "3-Jan-2006 10:00:00 +0000", size: 22,
					subject: "Unread one", fromName: "Bob", fromAddr: "bob@example.com", msgID: "<b@x>",
				},
			},
		},
		{
			name: "&ZeVnLIqe-", attrs: []string{"\\HasNoChildren"}, delim: "/", selectable: true, uidvalidity: 43,
			msgs: []*fakeMsg{
				{
					uid: 1, date: "4-Jan-2006 12:00:00 +0000", size: 33,
					subject: "CJK box", fromAddr: "c@example.com", msgID: "<c@x>",
				},
			},
		},
	}
}

func TestHandlerMailboxesAndSync(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)
	base := fmt.Sprintf("%s/%d", AccountsPrefix, id)

	// Before any sync: empty list, and GET-one carries an empty array (not
	// the M1 placeholder shape).
	if rr := e.serve(t, "alice", http.MethodGet, base+"/mailboxes", ""); rr.Code != http.StatusOK ||
		rr.Body.String() != "[]" {
		t.Fatalf("mailboxes before sync: status = %d body = %s", rr.Code, rr.Body.String())
	}

	seedSyncFake(e)

	// POST sync runs the pass synchronously and reports the inserted count.
	rr := e.serve(t, "alice", http.MethodPost, base+"/sync", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("sync: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var syncResp struct {
		NewMessages int `json:"newMessages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &syncResp); err != nil {
		t.Fatal(err)
	}
	if syncResp.NewMessages != 3 {
		t.Errorf("newMessages = %d, want 3", syncResp.NewMessages)
	}

	// The mailbox list carries counts and mUTF7-DECODED display names.
	rr = e.serve(t, "alice", http.MethodGet, base+"/mailboxes", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("mailboxes: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var boxes []mailboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &boxes); err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 2 {
		t.Fatalf("boxes = %+v", boxes)
	}
	if boxes[0].Name != "INBOX" || boxes[0].Total != 2 || boxes[0].Unread != 1 ||
		!boxes[0].Selectable || boxes[0].ID <= 0 || boxes[0].SpecialUse != "" {
		t.Errorf("inbox = %+v", boxes[0])
	}
	if boxes[1].Name != "日本語" || boxes[1].Total != 1 || boxes[1].Unread != 1 {
		t.Errorf("cjk box = %+v (wire name must decode)", boxes[1])
	}

	// GET-one-account embeds the same synced list.
	rr = e.serve(t, "alice", http.MethodGet, base, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get: status = %d", rr.Code)
	}
	var detail accountDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Mailboxes) != 2 || detail.Mailboxes[0].Name != "INBOX" || detail.Mailboxes[0].Unread != 1 ||
		detail.Mailboxes[1].Name != "日本語" {
		t.Errorf("get mailboxes = %+v", detail.Mailboxes)
	}

	// A second sync inserts nothing.
	rr = e.serve(t, "alice", http.MethodPost, base+"/sync", "")
	if err := json.Unmarshal(rr.Body.Bytes(), &syncResp); err != nil {
		t.Fatal(err)
	}
	if syncResp.NewMessages != 0 {
		t.Errorf("second sync newMessages = %d", syncResp.NewMessages)
	}
}

func TestHandlerSyncFailure502(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)
	base := fmt.Sprintf("%s/%d", AccountsPrefix, id)
	seedSyncFake(e)
	e.sfake.failList = true
	rr := e.serve(t, "alice", http.MethodPost, base+"/sync", "")
	if rr.Code != http.StatusBadGateway {
		t.Errorf("sync with a broken server: status = %d body = %s, want 502", rr.Code, rr.Body.String())
	}
}

func TestHandlerSyncAndMailboxesCrossUser404(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)
	base := fmt.Sprintf("%s/%d", AccountsPrefix, id)
	seedSyncFake(e)
	// The real endpoints, cross-user: 404, never 403.
	if rr := e.serve(t, "bob", http.MethodGet, base+"/mailboxes", ""); rr.Code != http.StatusNotFound {
		t.Errorf("bob GET mailboxes: status = %d, want 404", rr.Code)
	}
	if rr := e.serve(t, "bob", http.MethodPost, base+"/sync", ""); rr.Code != http.StatusNotFound {
		t.Errorf("bob POST sync: status = %d, want 404", rr.Code)
	}
	// Method mismatches on the sub-resources are 405 for everyone (the
	// method, not the account, is wrong).
	if rr := e.serve(t, "alice", http.MethodPut, base+"/sync", "{}"); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT sync: status = %d, want 405", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodDelete, base+"/mailboxes", ""); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE mailboxes: status = %d, want 405", rr.Code)
	}
	// Unknown sub-resources are 404.
	if rr := e.serve(t, "alice", http.MethodGet, base+"/messages", ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown sub-resource: status = %d, want 404", rr.Code)
	}
}
