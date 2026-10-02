package imap

import (
	"context"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"
)

// append_test.go covers the M5 APPEND command (ADR-0108 §1) against the
// scripted fake: the classic continuation exchange, the LITERAL+ fast path,
// refusals before/after the payload, the guards firing before any write
// (client stays usable), and the one-continuation limit.

var appendDate = time.Date(2006, time.January, 2, 15, 4, 5, 0, time.UTC)

// readLiteral reads exactly n payload bytes plus the CRLF terminating the
// command, returning the payload.
func (s *fakeSession) readLiteral(n int) []byte {
	s.t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.r, buf); err != nil {
		s.t.Errorf("fake: read literal of %d bytes: %v", n, err)
		return nil
	}
	crlf := make([]byte, 2)
	if _, err := io.ReadFull(s.r, crlf); err != nil || string(crlf) != "\r\n" {
		s.t.Errorf("fake: literal terminator = %q, want CRLF (err=%v)", crlf, err)
	}
	return buf
}

func TestAppendClassicContinuation(t *testing.T) {
	t.Parallel()
	payload := []byte("Subject: hi\r\n\r\nbody")
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`APPEND "Sent Items" (\Seen) "2-Jan-2006 15:04:05 +0000" {` + strconv.Itoa(len(payload)) + `}`)
		// An untagged line during the wait is collected and ignored.
		s.line("* 99 EXISTS")
		s.line("+ go ahead")
		if got := s.readLiteral(len(payload)); string(got) != string(payload) {
			s.t.Errorf("literal payload = %q, want %q", got, payload)
		}
		s.line(tag + " OK append done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	if err := c.Append(context.Background(), "Sent Items", []string{`\Seen`}, appendDate, payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestAppendLiteralPlusSkipsContinuation(t *testing.T) {
	t.Parallel()
	payload := []byte("Subject: plus\r\n\r\nbody")
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1 LITERAL+")
		s.line(tag + " OK done")
		tag = s.expectCmd(`APPEND "Sent" (\Seen) "2-Jan-2006 15:04:05 +0000" {` + strconv.Itoa(len(payload)) + `+}`)
		// LITERAL+: the payload follows the command line with NO "+ ".
		if got := s.readLiteral(len(payload)); string(got) != string(payload) {
			s.t.Errorf("literal payload = %q, want %q", got, payload)
		}
		s.line(tag + " OK append done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	if err := c.Append(context.Background(), "Sent", []string{`\Seen`}, appendDate, payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

// TestAppendRefusedBeforeContinuation proves a tagged NO instead of the
// continuation aborts the upload: not one payload byte crosses (the fake
// sees EOF right after its NO because the refusal breaks the client).
func TestAppendRefusedBeforeContinuation(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`APPEND "Gone" (\Seen) "2-Jan-2006 15:04:05 +0000" {4}`)
		s.line(tag + " NO no such mailbox")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	err := c.Append(context.Background(), "Gone", []string{`\Seen`}, appendDate, []byte("data"))
	if !errors.Is(err, ErrCommandRefused) {
		t.Fatalf("Append NO: err = %v, want ErrCommandRefused", err)
	}
	wait()
}

func TestAppendRefusedAfterData(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`APPEND "Sent" () "2-Jan-2006 15:04:05 +0000" {4}`)
		s.line("+ go ahead")
		s.readLiteral(4)
		s.line(tag + " NO message too big")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	err := c.Append(context.Background(), "Sent", nil, appendDate, []byte("data"))
	if !errors.Is(err, ErrCommandRefused) {
		t.Fatalf("Append NO after data: err = %v, want ErrCommandRefused", err)
	}
	wait()
}

func TestAppendBadIsFatal(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`APPEND "Sent" (\Seen) "2-Jan-2006 15:04:05 +0000" {4}`)
		s.line(tag + " BAD command unknown")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	err := c.Append(context.Background(), "Sent", []string{`\Seen`}, appendDate, []byte("data"))
	if err == nil || errors.Is(err, ErrCommandRefused) {
		t.Fatalf("Append BAD: err = %v, want a fatal protocol error", err)
	}
	wait()
}

// TestAppendSecondContinuationIsFatal pins the one-continuation limit: a
// second "+ " after the payload is the same protocol violation roundTrip
// enforces for every other command.
func TestAppendSecondContinuationIsFatal(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		s.expectCmd(`APPEND "Sent" (\Seen) "2-Jan-2006 15:04:05 +0000" {4}`)
		s.line("+ go ahead")
		s.readLiteral(4)
		s.line("+ again")
		s.expectEOF()
	})
	c := dialLogged(t, dial)
	if err := c.Append(context.Background(), "Sent", []string{`\Seen`}, appendDate, []byte("data")); err == nil {
		t.Fatal("Append with a second continuation: err = nil, want a fatal protocol error")
	}
	wait()
}

// TestAppendGuardsBeforeWrite proves the size cap, the flag allowlist, and
// the name injection guard all fire before anything is written — the client
// stays usable (a following NOOP succeeds).
func TestAppendGuardsBeforeWrite(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("NOOP")
		s.line(tag + " OK noop done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c := dialLogged(t, dial)
	if err := c.Append(context.Background(), "Sent", []string{`\Seen`}, appendDate, make([]byte, maxAppendSize+1)); !errors.Is(err, ErrAppendTooLarge) {
		t.Errorf("oversize: err = %v, want ErrAppendTooLarge", err)
	}
	if err := c.Append(context.Background(), "Sent", []string{`\Seen", "x`}, appendDate, []byte("x")); !errors.Is(err, ErrInvalidFlag) {
		t.Errorf("flag breakout: err = %v, want ErrInvalidFlag", err)
	}
	if err := c.Append(context.Background(), "Sent\r\nA1 NOOP", nil, appendDate, []byte("x")); !errors.Is(err, ErrInjection) {
		t.Errorf("name injection: err = %v, want ErrInjection", err)
	}
	if err := c.Noop(); err != nil {
		t.Errorf("Noop after the guards: %v (client must stay usable)", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}
