package mail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"testing"
)

// handler_send_test.go covers the M5 send endpoint (ADR-0108 §1) through
// the real auth middleware, the real Sender, and the scripted fakes: the
// full happy path (envelope, DATA bytes, AUTH with the SMTP half of the
// sealed pair, Sent APPEND with \Seen), the failure taxonomy (append-failure
// still 200, recipient refusal 400, auth failure 502), the validation
// matrix, cross-user 404, and the {path} attachment form.

// upsertSent gives the account a synced Sent mailbox (special_use='sent')
// locally AND registers it with the fake IMAP server (APPEND lands there).
func upsertSent(t *testing.T, e *mailEnv, accountID int64) {
	t.Helper()
	if err := e.store.UpsertMailbox(t.Context(), &Mailbox{
		AccountID: accountID, Name: "Sent", SpecialUse: "sent", Selectable: true,
	}); err != nil {
		t.Fatal(err)
	}
	e.sfake.boxes = append(e.sfake.boxes, &fakeMailbox{name: "Sent", delim: "/", selectable: true, uidvalidity: 1})
}

func TestHandlerSendHappyPath(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)
	upsertSent(t, e, id)

	rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix+"/1/send", `{
		"to": ["Bob <bob@example.com>"],
		"cc": ["carol@example.com"],
		"bcc": ["dave@example.com"],
		"subject": "Quarterly report 数据",
		"bodyPlain": "see attached",
		"attachments": [{"filename": "note.txt", "contentType": "text/plain", "contentBase64": "aGVsbG8="}]
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("send: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^<[^<>]+@example\.com>$`).MatchString(resp.MessageID) {
		t.Errorf("messageId = %q", resp.MessageID)
	}

	// The SMTP envelope: from the account email; the Bcc recipient reaches
	// RCPT (and only RCPT).
	from, rcpts := e.smtp.envelope()
	if from != "alice@example.com" {
		t.Errorf("MAIL FROM = %q", from)
	}
	if strings.Join(rcpts, ",") != "bob@example.com,carol@example.com,dave@example.com" {
		t.Errorf("RCPT set = %v, want to+cc+bcc", rcpts)
	}
	data := e.smtp.payload()
	// The non-ASCII subject round-trips: encoded on the wire, decoding back
	// to the original.
	wire, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DATA payload does not parse as a message: %v", err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(wire.Header.Get("Subject"))
	if err != nil || subject != "Quarterly report 数据" {
		t.Errorf("decoded subject = %q (err=%v)", subject, err)
	}
	if wire.Header.Get("Bcc") != "" {
		t.Error("Bcc header present in the DATA bytes")
	}
	if !strings.Contains(string(data), "see attached") {
		t.Error("DATA lacks the plain body")
	}
	if !strings.Contains(string(data), "aGVsbG8=") {
		t.Error("DATA lacks the base64 attachment content")
	}
	if bytes.Contains(data, []byte("dave@example.com")) {
		t.Error("Bcc address leaked into the DATA bytes")
	}
	// AUTH carried the SMTP half of the sealed credential pair.
	if auths := e.smtp.authLog(); len(auths) != 1 || auths[0] != [2]string{"alice@example.com", "smtp-secret-pw"} {
		t.Errorf("auths = %v, want the account's smtp credentials", auths)
	}

	// Save to Sent: the fake IMAP got the APPEND with \Seen and the same
	// composed bytes (subject inside).
	sent := e.sfake.mailbox("Sent")
	if sent == nil || len(sent.msgs) != 1 {
		t.Fatalf("Sent mailbox msgs = %+v, want the 1 appended copy", sent)
	}
	appended := sent.msgs[0]
	if !bytes.Contains(appended.raw, []byte("see attached")) {
		t.Error("Sent copy lacks the body")
	}
	if !hasWireFlag(appended, "\\Seen") {
		t.Errorf("Sent copy flags = %v, want \\Seen", appended.flags)
	}
	var sawAppend bool
	for _, op := range e.sfake.opsLog() {
		if op == "APPEND Sent" {
			sawAppend = true
		}
	}
	if !sawAppend {
		t.Errorf("ops log = %v, want APPEND Sent", e.sfake.opsLog())
	}
}

func TestHandlerSendAppendFailureStill200(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)
	upsertSent(t, e, id)
	e.sfake.setFailApnd(true)
	var hookErr error
	e.sender.OnAppendError = func(err error) { hookErr = err }

	rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix+"/1/send",
		`{"to": ["bob@example.com"], "subject": "hi", "bodyPlain": "x"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("send: status = %d body = %s (an append failure must not fail the send)", rr.Code, rr.Body.String())
	}
	if !errors.Is(hookErr, ErrSentAppend) {
		t.Errorf("hook err = %v, want ErrSentAppend", hookErr)
	}
	// The send itself did cross the SMTP wire.
	if _, rcpts := e.smtp.envelope(); strings.Join(rcpts, ",") != "bob@example.com" {
		t.Errorf("RCPT set = %v", rcpts)
	}
}

func TestHandlerSendNoSentMailbox(t *testing.T) {
	e := newMailEnv(t)
	createAlice(t, e) // no Sent mailbox synced
	rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix+"/1/send",
		`{"to": ["bob@example.com"], "subject": "hi", "bodyPlain": "x"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("send: status = %d body = %s (no Sent mailbox is not an error)", rr.Code, rr.Body.String())
	}
	for _, op := range e.sfake.opsLog() {
		if strings.HasPrefix(op, "APPEND") {
			t.Errorf("ops log = %v: APPEND without a Sent mailbox", e.sfake.opsLog())
		}
	}
}

func TestHandlerSendRecipientRefused(t *testing.T) {
	e := newMailEnv(t)
	createAlice(t, e)
	e.smtp.setRefuseRcpt(map[string]bool{"carol@example.com": true})
	rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix+"/1/send",
		`{"to": ["carol@example.com"], "subject": "hi", "bodyPlain": "x"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("send: status = %d body = %s, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "carol@example.com") {
		t.Errorf("400 body must name the refused address: %s", rr.Body.String())
	}
}

func TestHandlerSendSMTPAuthFailure(t *testing.T) {
	e := newMailEnv(t)
	createAlice(t, e)
	e.smtp.setAcceptAuth(func(_, _ string) bool { return false })
	rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix+"/1/send",
		`{"to": ["bob@example.com"], "subject": "hi", "bodyPlain": "x"}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("send: status = %d body = %s, want 502", rr.Code, rr.Body.String())
	}
}

func TestHandlerSendValidation(t *testing.T) {
	e := newMailEnv(t)
	createAlice(t, e)
	e.readFiles["/Documents/report.pdf"] = []byte("%PDF-fake")
	bigB64 := base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentSize+1))
	many := make([]string, 0, maxOutgoingAttachments+1)
	for i := 0; i <= maxOutgoingAttachments; i++ {
		many = append(many, `{"filename": "f.txt", "contentBase64": "eA=="}`)
	}
	cases := []struct {
		name string
		body string
		want string // substring of the 400 message ("" = no assertion)
	}{
		{"no recipients", `{"subject": "hi", "bodyPlain": "x"}`, "recipient"},
		{"no body", `{"to": ["bob@example.com"], "subject": "hi"}`, "body"},
		{"bad to address", `{"to": ["not an address"], "bodyPlain": "x"}`, "to"},
		{"bad cc address", `{"cc": ["not an address"], "bodyPlain": "x"}`, "cc"},
		{"bad b64", `{"to": ["bob@example.com"], "bodyPlain": "x", "attachments": [{"filename": "f", "contentBase64": "!!!"}]}`, "base64"},
		{"both path and b64", `{"to": ["bob@example.com"], "bodyPlain": "x", "attachments": [{"filename": "f", "contentBase64": "eA==", "path": "/Documents/report.pdf"}]}`, "exactly one"},
		{"neither path nor b64", `{"to": ["bob@example.com"], "bodyPlain": "x", "attachments": [{"filename": "f"}]}`, "exactly one"},
		{"unreadable path", `{"to": ["bob@example.com"], "bodyPlain": "x", "attachments": [{"path": "/nope.pdf"}]}`, "cannot read"},
		{"too many attachments", `{"to": ["bob@example.com"], "bodyPlain": "x", "attachments": [` + strings.Join(many, ",") + `]}`, "attachments"},
		{"attachment too large", `{"to": ["bob@example.com"], "bodyPlain": "x", "attachments": [{"filename": "big", "contentBase64": "` + bigB64 + `"}]}`, "10 MiB"},
		{"subject injection", `{"to": ["bob@example.com"], "subject": "hi\r\nBcc: evil@example.com", "bodyPlain": "x"}`, ""},
		{"bad inReplyTo", `{"to": ["bob@example.com"], "inReplyTo": "not-an-id", "bodyPlain": "x"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix+"/1/send", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s, want 400", rr.Code, rr.Body.String())
			}
			if tc.want != "" && !strings.Contains(rr.Body.String(), tc.want) {
				t.Errorf("body = %s, want substring %q", rr.Body.String(), tc.want)
			}
		})
	}
}

func TestHandlerSendCrossUser(t *testing.T) {
	e := newMailEnv(t)
	createAlice(t, e)
	rr := e.serve(t, "bob", http.MethodPost, AccountsPrefix+"/1/send",
		`{"to": ["bob@example.com"], "subject": "hi", "bodyPlain": "x"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-user send: status = %d body = %s, want 404", rr.Code, rr.Body.String())
	}
	if _, rcpts := e.smtp.envelope(); len(rcpts) != 0 {
		t.Errorf("cross-user send reached the SMTP wire: %v", rcpts)
	}
}

func TestHandlerSendPathAttachment(t *testing.T) {
	e := newMailEnv(t)
	createAlice(t, e)
	e.readFiles["/Documents/report.pdf"] = []byte("%PDF-fake")
	rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix+"/1/send", `{
		"to": ["bob@example.com"],
		"subject": "the report",
		"bodyPlain": "attached",
		"attachments": [{"path": "/Documents/report.pdf", "contentType": "application/pdf"}]
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("send: status = %d body = %s", rr.Code, rr.Body.String())
	}
	data := string(e.smtp.payload())
	if !strings.Contains(data, base64.StdEncoding.EncodeToString([]byte("%PDF-fake"))) {
		t.Error("DATA lacks the base64 of the file content")
	}
	if !strings.Contains(data, "report.pdf") {
		t.Error("DATA lacks the derived filename (report.pdf)")
	}
}

func TestHandlerSendMethodGate(t *testing.T) {
	e := newMailEnv(t)
	createAlice(t, e)
	rr := e.serve(t, "alice", http.MethodGet, AccountsPrefix+"/1/send", "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET send: status = %d, want 405", rr.Code)
	}
}
