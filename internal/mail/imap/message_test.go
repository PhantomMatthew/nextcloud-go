package imap

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// message_test.go covers the M4 live-op commands (ADR-0108 §6) against the
// scripted fake server: command shapes asserted verbatim, NO/BAD mapped, and
// injection/allowlist rejections proven to happen before any write.

func TestSelectParsesLikeExamine(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`SELECT "Sent Items"`)
		s.line("* 7 EXISTS")
		s.line("* 0 RECENT")
		s.line(`* OK [UIDVALIDITY 4242] UIDs valid`)
		s.line(`* OK [UIDNEXT 8] Predicted next UID`)
		s.line(tag + " OK [READ-WRITE] select done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	sr, err := c.Select(context.Background(), "Sent Items")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	want := SelectResult{Exists: 7, UIDValidity: 4242, UIDNext: 8}
	if sr != want {
		t.Errorf("SelectResult = %+v, want %+v", sr, want)
	}
}

func TestSelectRefusedIsCommandRefused(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`SELECT "INBOX"`)
		s.line(tag + " NO no such mailbox")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	if _, err := c.Select(context.Background(), "INBOX"); !errors.Is(err, ErrCommandRefused) {
		t.Fatalf("Select NO: err = %v", err)
	}
	wait()
}

// TestUIDFetchFullLiteral proves the reader counts literal octets instead of
// rescanning: the BODY[] payload carries CRLFs, NUL bytes, and a "{4}"-plus-
// CRLF sequence that would desync a naive marker scan.
func TestUIDFetchFullLiteral(t *testing.T) {
	t.Parallel()
	payload := "Subject: hi\r\n\r\nline1\r\n\x00binary {4}\r\nstill body {123456}\r\n"
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("UID FETCH 42 (UID BODY.PEEK[])")
		// An unsolicited FLAGS update for another message rides along and
		// must be skipped; the real answer carries the literal.
		s.line(`* 9 FETCH (UID 41 FLAGS (\Seen))`)
		s.raw("* 1 FETCH (UID 42 BODY[] {" + strconv.Itoa(len(payload)) + "}\r\n" + payload + ")\r\n")
		s.line(tag + " OK fetch done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	raw, err := c.UIDFetchFull(context.Background(), 42)
	if err != nil {
		t.Fatalf("UIDFetchFull: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if string(raw) != payload {
		t.Errorf("body = %q, want %q", raw, payload)
	}
}

// TestUIDFetchFullGone: an OK answer with no data for the uid means the
// message is gone — (nil, nil), not an error.
func TestUIDFetchFullGone(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("UID FETCH 42 (UID BODY.PEEK[])")
		s.line(tag + " OK fetch done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	raw, err := c.UIDFetchFull(context.Background(), 42)
	if err != nil {
		t.Fatalf("UIDFetchFull: %v", err)
	}
	if raw != nil {
		t.Errorf("gone message body = %q, want nil", raw)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestUIDFetchFullRefused(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("UID FETCH 42 (UID BODY.PEEK[])")
		s.line(tag + " NO fetch broken")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	if _, err := c.UIDFetchFull(context.Background(), 42); err == nil {
		t.Fatal("UIDFetchFull NO succeeded")
	}
	wait()
}

func TestUIDStoreShapes(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`UID STORE 1,2 +FLAGS.SILENT (\Seen)`)
		s.line(tag + " OK store done")
		tag = s.expectCmd(`UID STORE 1,2 -FLAGS.SILENT (\Flagged $Label1)`)
		s.line(tag + " OK store done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	if err := c.UIDStore(context.Background(), []uint64{1, 2}, []string{`\Seen`}, []string{`\Flagged`, "$Label1"}); err != nil {
		t.Fatalf("UIDStore: %v", err)
	}
	// Both sides empty: a no-op, nothing on the wire.
	if err := c.UIDStore(context.Background(), []uint64{1, 2}, nil, nil); err != nil {
		t.Fatalf("UIDStore no-op: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if got := strings.Join(s.cmds, "|"); got != "CAPABILITY|UID STORE 1,2 +FLAGS.SILENT (\\Seen)|UID STORE 1,2 -FLAGS.SILENT (\\Flagged $Label1)|LOGOUT" {
		t.Errorf("commands = %q", got)
	}
}

// TestUIDStoreOneSide: an empty add or remove side skips that command.
func TestUIDStoreOneSide(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`UID STORE 7 -FLAGS.SILENT (\Deleted)`)
		s.line(tag + " OK store done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	if err := c.UIDStore(context.Background(), []uint64{7}, nil, []string{`\Deleted`}); err != nil {
		t.Fatalf("UIDStore: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if got := strings.Join(s.cmds, "|"); got != `CAPABILITY|UID STORE 7 -FLAGS.SILENT (\Deleted)|LOGOUT` {
		t.Errorf("commands = %q", got)
	}
}

func TestUIDStoreRefused(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`UID STORE 7 +FLAGS.SILENT (\Seen)`)
		s.line(tag + " NO store broken")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	if err := c.UIDStore(context.Background(), []uint64{7}, []string{`\Seen`}, nil); !errors.Is(err, ErrCommandRefused) {
		t.Fatalf("UIDStore NO: err = %v", err)
	}
	wait()
}

// TestUIDStoreAllowlistRejectedBeforeWrite: anything outside the allowlist —
// CR/LF breakouts, list breakouts, spaces, lowercase system names — is
// ErrInvalidFlag and never reaches the wire; the client stays usable.
func TestUIDStoreAllowlistRejectedBeforeWrite(t *testing.T) {
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
	bad := []string{
		`\Seen\r\na1 EXPUNGE`, `\\Evil`, `\seen`, "", `\Seen) (\\Deleted`,
		"$", "$label with space", "Flag With Space", `"quoted"`, `\Recent1`, "$Label-1",
	}
	for _, f := range bad {
		if err := c.UIDStore(context.Background(), []uint64{1}, []string{f}, nil); !errors.Is(err, ErrInvalidFlag) {
			t.Errorf("add %q: err = %v, want ErrInvalidFlag", f, err)
		}
		if err := c.UIDStore(context.Background(), []uint64{1}, nil, []string{f}); !errors.Is(err, ErrInvalidFlag) {
			t.Errorf("remove %q: err = %v, want ErrInvalidFlag", f, err)
		}
	}
	// Bare atoms are keywords, so even "seen"/"SEEN" pass as keywords (only
	// the backslash-prefixed system form is case-pinned).
	good := []string{
		`\Seen`, `\Answered`, `\Flagged`, `\Deleted`, `\Draft`, `\Recent`,
		"Seen", "seen", "SEEN", "$Label1", "$MDNSent", "Keyword", "kw_2",
	}
	for _, f := range good {
		// Validate only: use an empty uid set so nothing hits the wire.
		if err := c.UIDStore(context.Background(), nil, []string{f}, nil); err != nil {
			t.Errorf("allowlisted %q: err = %v", f, err)
		}
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout after rejections: %v", err)
	}
	wait()
	if got := strings.Join(s.cmds, "|"); got != "CAPABILITY|LOGOUT" {
		t.Errorf("rejected flags must never reach the wire, commands = %q", got)
	}
}

func TestAllowedFlagMatrix(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		`\Seen`: true, `\Answered`: true, `\Flagged`: true, `\Deleted`: true, `\Draft`: true, `\Recent`: true,
		"Seen": true, "Draft": true, "seen": true, "SEEN": true, // bare atoms are keywords
		"$Label1": true, "$Junk": true, "NotJunk": true, "a": true, "A0_z": true,
		"\\seen": false, `\SEEN`: false, `\RecentX`: false, "$": false, "": false,
		"x y": false, "x\ty": false, "(x)": false, `x"y`: false, "$x-y": false, "é": false,
	}
	for f, want := range cases {
		if got := allowedFlag(f); got != want {
			t.Errorf("allowedFlag(%q) = %v, want %v", f, got, want)
		}
	}
}

func TestUIDCopyShape(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`UID COPY 1,2 "Sent Items"`)
		s.line(tag + " OK copy done")
		tag = s.expectCmd(`UID COPY 3 "Quo\"ted"`)
		s.line(tag + " OK copy done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	if err := c.UIDCopy(context.Background(), []uint64{1, 2}, "Sent Items"); err != nil {
		t.Fatalf("UIDCopy: %v", err)
	}
	// The destination passes through quoteString: quotes are escaped.
	if err := c.UIDCopy(context.Background(), []uint64{3}, `Quo"ted`); err != nil {
		t.Fatalf("UIDCopy quoted: %v", err)
	}
	// An empty uid set is a no-op.
	if err := c.UIDCopy(context.Background(), nil, "INBOX"); err != nil {
		t.Fatalf("UIDCopy empty: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if got := strings.Join(s.cmds, "|"); got != `CAPABILITY|UID COPY 1,2 "Sent Items"|UID COPY 3 "Quo\"ted"|LOGOUT` {
		t.Errorf("commands = %q", got)
	}
}

func TestUIDCopyRefusedAndInjection(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`UID COPY 7 "Trash"`)
		s.line(tag + " NO no such mailbox")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	// Injection in the destination is rejected before any write (and the
	// client stays usable — a CR/LF never reaches quoteString's output).
	if err := c.UIDCopy(context.Background(), []uint64{7}, "Trash\"\r\na1 EXPUNGE \""); !errors.Is(err, ErrInjection) {
		t.Fatalf("UIDCopy injection: err = %v", err)
	}
	if err := c.UIDCopy(context.Background(), []uint64{7}, "Trash"); !errors.Is(err, ErrCommandRefused) {
		t.Fatalf("UIDCopy NO: err = %v", err)
	}
	wait()
	if got := strings.Join(s.cmds, "|"); got != `CAPABILITY|UID COPY 7 "Trash"` {
		t.Errorf("commands = %q", got)
	}
}

func TestExpunge(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("EXPUNGE")
		s.line("* 3 EXPUNGE")
		s.line("* 2 EXPUNGE")
		s.line(tag + " OK expunge done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	if err := c.Expunge(context.Background()); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestExpungeRefused(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("EXPUNGE")
		s.line(tag + " NO expunge broken")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	if err := c.Expunge(context.Background()); !errors.Is(err, ErrCommandRefused) {
		t.Fatalf("Expunge NO: err = %v", err)
	}
	wait()
}
