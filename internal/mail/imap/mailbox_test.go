package imap

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// dialLogged dials the fake, runs CAPABILITY, and hands the client to the
// test body; the script must have played out the greeting/CAPABILITY lines.
func dialLogged(t *testing.T, dial func(ctx context.Context, network, address string) (net.Conn, error)) *Client {
	t.Helper()
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

func TestListShapes(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`LIST "" "*"`)
		s.line(`* LIST (\HasNoChildren) "/" INBOX`)
		s.line(`* LIST (\HasNoChildren \Sent) "/" "Sent"`)
		s.line(`* LIST (\Noselect \HasChildren) "/" "[Gmail]"`)
		s.line(`* LIST (\HasNoChildren) NIL "Sent Items"`)
		s.line(`* LIST () "." &ZeVnLIqe-`)
		s.line(tag + " OK list done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	boxes, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	want := []MailboxInfo{
		{WireName: "INBOX", Delim: "/", Attrs: []string{"\\HasNoChildren"}, Selectable: true},
		{WireName: "Sent", Delim: "/", Attrs: []string{"\\HasNoChildren", "\\Sent"}, Selectable: true},
		{WireName: "[Gmail]", Delim: "/", Attrs: []string{"\\Noselect", "\\HasChildren"}, Selectable: false},
		{WireName: "Sent Items", Delim: "", Attrs: []string{"\\HasNoChildren"}, Selectable: true},
		{WireName: "&ZeVnLIqe-", Delim: ".", Attrs: nil, Selectable: true},
	}
	if !reflect.DeepEqual(boxes, want) {
		t.Errorf("boxes = %#v\nwant %#v", boxes, want)
	}
}

func TestListMalformedLineIsFatal(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`LIST "" "*"`)
		s.line(`* LIST (\HasNoChildren "/" INBOX`) // unbalanced paren
		s.line(tag + " OK list done")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("List with a malformed line succeeded")
	}
	wait()
	if !s.SawEOF {
		t.Error("client did not close the connection after a malformed LIST line")
	}
}

func TestExamineParsesResponseCodes(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`EXAMINE "Sent Items"`)
		s.line("* 23 EXISTS")
		s.line("* 0 RECENT")
		s.line(`* OK [UIDVALIDITY 3857529045] UIDs valid`)
		s.line(`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`)
		s.line(`* OK [PERMANENTFLAGS ()] Read-only mailbox`)
		s.line(`* OK [UIDNEXT 4392] Predicted next UID`)
		s.line(`* OK [READ-ONLY] Mailbox selected read-only`)
		s.line(tag + " OK examine done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	sr, err := c.Examine(context.Background(), "Sent Items")
	if err != nil {
		t.Fatalf("Examine: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	want := SelectResult{Exists: 23, UIDValidity: 3857529045, UIDNext: 4392}
	if sr != want {
		t.Errorf("SelectResult = %+v, want %+v", sr, want)
	}
}

func TestExamineRefused(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`EXAMINE "INBOX"`)
		s.line(tag + " NO no such mailbox")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	if _, err := c.Examine(context.Background(), "INBOX"); err == nil {
		t.Fatal("Examine NO succeeded")
	}
	wait()
}

func TestExamineInjectionRejected(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	if _, err := c.Examine(context.Background(), "INBOX\"\r\na1 DELETE \"x"); !errors.Is(err, ErrInjection) {
		t.Fatalf("Examine injection: err = %v", err)
	}
	// The rejection happens before any write, so the client stays usable.
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout after injection rejection: %v", err)
	}
	wait()
}

func TestUIDSearchAll(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("UID SEARCH ALL")
		s.line("* SEARCH 1 2 5 42 100")
		s.line(tag + " OK search done")
		tag = s.expectCmd("UID SEARCH ALL")
		s.line("* SEARCH")
		s.line(tag + " OK search done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	uids, err := c.UIDSearchAll(context.Background())
	if err != nil {
		t.Fatalf("UIDSearchAll: %v", err)
	}
	if !reflect.DeepEqual(uids, []uint64{1, 2, 5, 42, 100}) {
		t.Errorf("uids = %v", uids)
	}
	empty, err := c.UIDSearchAll(context.Background())
	if err != nil {
		t.Fatalf("UIDSearchAll empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty search = %v", empty)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestUIDFetchSummariesMatrix(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("UID FETCH 5,7,9,11 (UID FLAGS INTERNALDATE RFC822.SIZE ENVELOPE)")
		// Full shape: encoded-word subject preserved raw (the mail layer
		// decodes RFC 2047), one from, two tos.
		s.line(`* 1 FETCH (UID 5 FLAGS (\Seen \Flagged) INTERNALDATE "2-Jan-2006 15:04:05 -0700" RFC822.SIZE 4567 ENVELOPE ("Mon, 2 Jan 2006 15:04:05 -0700" "=?UTF-8?B?5pel5pys6Kqe?=" (("=?UTF-8?Q?J=C3=BCrgen_M=C3=BCller?=" NIL "jmueller" "example.com")) NIL NIL (("T" NIL "to1" "example.com") (NIL NIL "to2" "example.org")) NIL NIL NIL "<msg-5@example.com>"))`) //nolint:dupword // ENVELOPE wire shape carries NIL runs
		// NIL from list, empty to list, NIL subject, NIL message-id.
		s.line(`* 2 FETCH (UID 7 FLAGS () INTERNALDATE "12-Feb-2006 01:02:03 +0530" RFC822.SIZE 12 ENVELOPE (NIL NIL NIL NIL NIL () NIL NIL NIL NIL))`) //nolint:dupword // ENVELOPE wire shape carries NIL runs
		// Group-syntax address entries (NIL mailbox/host) are skipped.
		s.line(`* 3 FETCH (UID 9 FLAGS (\Draft) INTERNALDATE "3-Mar-2006 10:00:00 +0000" RFC822.SIZE 999 ENVELOPE ("d" "group test" (("Boss" NIL "boss" "example.com")) NIL NIL ((NIL NIL "undisclosed" NIL) ("Real" NIL "real" "example.com") (NIL NIL NIL NIL)) NIL NIL NIL "<g@x>"))`) //nolint:dupword // ENVELOPE wire shape carries NIL runs
		// A non-FETCH untagged line rides along and is ignored.
		s.line("* 44 EXISTS")
		s.line(tag + " OK fetch done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	sums, err := c.UIDFetchSummaries(context.Background(), []uint64{5, 7, 9, 11})
	if err != nil {
		t.Fatalf("UIDFetchSummaries: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if len(sums) != 3 {
		t.Fatalf("summaries = %+v", sums)
	}
	first := sums[0]
	if first.UID != 5 || first.Size != 4567 || !reflect.DeepEqual(first.Flags, []string{"Seen", "Flagged"}) {
		t.Errorf("first = %+v", first)
	}
	wantDate := time.Date(2006, 1, 2, 15, 4, 5, 0, time.FixedZone("", -7*3600))
	if !first.InternalDate.Equal(wantDate) {
		t.Errorf("first.InternalDate = %v, want %v", first.InternalDate, wantDate)
	}
	if first.Envelope.Subject != "=?UTF-8?B?5pel5pys6Kqe?=" {
		t.Errorf("subject must stay raw at wire level, got %q", first.Envelope.Subject)
	}
	if first.Envelope.MessageID != "<msg-5@example.com>" {
		t.Errorf("message-id = %q", first.Envelope.MessageID)
	}
	wantFrom := []EnvelopeAddress{{Name: "=?UTF-8?Q?J=C3=BCrgen_M=C3=BCller?=", Mailbox: "jmueller", Host: "example.com"}}
	if !reflect.DeepEqual(first.Envelope.From, wantFrom) {
		t.Errorf("from = %+v", first.Envelope.From)
	}
	wantTo := []EnvelopeAddress{{Name: "T", Mailbox: "to1", Host: "example.com"}, {Mailbox: "to2", Host: "example.org"}}
	if !reflect.DeepEqual(first.Envelope.To, wantTo) {
		t.Errorf("to = %+v", first.Envelope.To)
	}

	second := sums[1]
	if second.UID != 7 || second.Size != 12 || len(second.Flags) != 0 ||
		second.Envelope.Subject != "" || len(second.Envelope.From) != 0 || len(second.Envelope.To) != 0 ||
		second.Envelope.MessageID != "" {
		t.Errorf("second (nil lists) = %+v", second)
	}
	if second.InternalDate.Location().String() == "UTC" {
		t.Errorf("second.InternalDate lost its +0530 zone: %v", second.InternalDate)
	}

	third := sums[2]
	if third.UID != 9 || !reflect.DeepEqual(third.Flags, []string{"Draft"}) {
		t.Errorf("third = %+v", third)
	}
	// Group markers (NIL mailbox/host) skipped; the real address survives.
	wantThirdTo := []EnvelopeAddress{{Name: "Real", Mailbox: "real", Host: "example.com"}}
	if !reflect.DeepEqual(third.Envelope.To, wantThirdTo) {
		t.Errorf("third.To = %+v", third.Envelope.To)
	}
}

func TestUIDFetchSummariesMissingUIDSkipped(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("UID FETCH 1 (UID FLAGS INTERNALDATE RFC822.SIZE ENVELOPE)")
		s.line(`* 1 FETCH (FLAGS (\Seen) INTERNALDATE "2-Jan-2006 15:04:05 -0700" RFC822.SIZE 1 ENVELOPE (NIL "no uid" NIL NIL NIL NIL NIL NIL NIL NIL))`) //nolint:dupword // ENVELOPE wire shape carries NIL runs
		s.line(tag + " OK fetch done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	sums, err := c.UIDFetchSummaries(context.Background(), []uint64{1})
	if err != nil {
		t.Fatalf("UIDFetchSummaries: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if len(sums) != 0 {
		t.Errorf("a UID-less FETCH data line must be skipped, got %+v", sums)
	}
}

func TestUIDFetchFlags(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("UID FETCH 5,7 (UID FLAGS)")
		s.line(`* 1 FETCH (UID 5 FLAGS (\Seen \Answered))`)
		s.line(`* 2 FETCH (UID 7 FLAGS ())`)
		s.line(tag + " OK fetch done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	flags, err := c.UIDFetchFlags(context.Background(), []uint64{5, 7})
	if err != nil {
		t.Fatalf("UIDFetchFlags: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	want := map[uint64][]string{5: {"Seen", "Answered"}, 7: {}}
	if !reflect.DeepEqual(flags, want) {
		t.Errorf("flags = %v, want %v", flags, want)
	}
}

func TestUIDFetchEmptySetIsNoop(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	sums, err := c.UIDFetchSummaries(context.Background(), nil)
	if err != nil || len(sums) != 0 {
		t.Errorf("empty summaries = %v %v", sums, err)
	}
	flags, err := c.UIDFetchFlags(context.Background(), nil)
	if err != nil || len(flags) != 0 {
		t.Errorf("empty flags = %v %v", flags, err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if got := strings.Join(s.cmds, "|"); got != "CAPABILITY|LOGOUT" {
		t.Errorf("no UID FETCH must be sent for an empty set, commands = %q", got)
	}
}

func TestBracketedCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line, code, value string
		ok                bool
	}{
		{`* OK [UIDVALIDITY 42] UIDs valid`, "UIDVALIDITY", "42", true},
		{`* OK [UIDNEXT 4392]`, "UIDNEXT", "4392", true},
		{`* OK [READ-ONLY] Mailbox selected read-only`, "", "", false},
		{`* OK no bracket here`, "", "", false},
		{`* OK [ALERT] System [shutdown] soon`, "", "", false}, // value-less first code
		{`* 23 EXISTS`, "", "", false},
	}
	for _, tc := range cases {
		code, value, ok := bracketedCode(tc.line)
		if code != tc.code || value != tc.value || ok != tc.ok {
			t.Errorf("bracketedCode(%q) = %q %q %v, want %q %q %v", tc.line, code, value, ok, tc.code, tc.value, tc.ok)
		}
	}
}
