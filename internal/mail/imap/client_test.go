package imap

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDialGreetingOK(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK fake IMAP4rev1 server ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1 AUTH=PLAIN")
		s.line(tag + " OK capability done")
		tag = s.expectCmd("LOGOUT")
		s.line("* BYE logging out")
		s.line(tag + " OK logout done")
	})
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if !c.hasCap("IMAP4REV1") || !c.hasCap("AUTH=PLAIN") {
		t.Errorf("caps = %v", c.caps)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestDialGreetingPreauth(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* PREAUTH fake server, already authenticated")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK capability done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK logout done")
	})
	// M2 treats PREAUTH as ready (ADR-0108 §1).
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestDialGreetingBye(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* BYE server is shutting down")
		s.expectEOF()
	})
	if _, err := Dial(context.Background(), dialOptions(dial, "none")); err == nil {
		t.Fatal("Dial against a BYE greeting succeeded")
	}
	wait()
	if !s.SawEOF {
		t.Error("client did not close the connection after a BYE greeting")
	}
}

func TestCapabilityUppercasesAndCollects(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1 starttls auth=plain Idle")
		s.line(tag + " OK done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Dial fetched them; assert the cached set is upper-cased.
	for _, want := range []string{"IMAP4REV1", "STARTTLS", "AUTH=PLAIN", "IDLE"} {
		if !c.hasCap(want) {
			t.Errorf("caps %v missing %s", c.caps, want)
		}
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestLoginOK(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`LOGIN "alice@example.com" "s3cret"`)
		s.line(tag + " OK logged in")
		tag = s.expectCmd("LOGOUT")
		s.line("* BYE bye")
		s.line(tag + " OK done")
	})
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Login("alice@example.com", "s3cret"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if got := strings.Join(s.cmds, "|"); got != `CAPABILITY|LOGIN "alice@example.com" "s3cret"|LOGOUT` {
		t.Errorf("commands = %q", got)
	}
}

func TestLoginNoIsAuthentication(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`LOGIN "u" "wrong"`)
		s.line(tag + " NO [AUTHENTICATIONFAILED] invalid credentials")
		// A failed LOGIN closes the connection (ADR-0108 §1).
		s.expectEOF()
	})
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	err = c.Login("u", "wrong")
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("Login err = %v, want ErrAuthentication", err)
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("Login err lacks the server text: %v", err)
	}
	if err := c.Noop(); !errors.Is(err, errBroken) {
		t.Errorf("Noop after fatal Login err = %v, want errBroken", err)
	}
	// Logout on a broken client is an idempotent no-op.
	if err := c.Logout(); err != nil {
		t.Errorf("Logout on broken client = %v, want nil", err)
	}
	wait()
	if !s.SawEOF {
		t.Error("server did not see the connection close after Login NO")
	}
}

func TestLoginBadIsProtocolError(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd(`LOGIN "u" "p"`)
		s.line(tag + " BAD command unknown")
		s.expectEOF()
	})
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	err = c.Login("u", "p")
	if err == nil || errors.Is(err, ErrAuthentication) {
		t.Fatalf("Login BAD err = %v, want a non-authentication protocol error", err)
	}
	if err := c.Noop(); !errors.Is(err, errBroken) {
		t.Errorf("Noop after BAD = %v, want errBroken", err)
	}
	wait()
}

func TestLoginDisabledRefusesBeforeWrite(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1 LOGINDISABLED")
		s.line(tag + " OK done")
		// The client must never send LOGIN: the next command is NOOP (the
		// client proving it is still usable), then LOGOUT.
		tag = s.expectCmd("NOOP")
		s.line(tag + " OK noop done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Login("u", "p"); !errors.Is(err, ErrLoginDisabled) {
		t.Fatalf("Login err = %v, want ErrLoginDisabled", err)
	}
	// The refusal is a policy decision, not a fatal error.
	if err := c.Noop(); err != nil {
		t.Errorf("Noop after LOGINDISABLED refusal: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Errorf("Logout: %v", err)
	}
	wait()
	for _, cmd := range s.cmds {
		if strings.HasPrefix(cmd, "LOGIN") {
			t.Errorf("client sent LOGIN despite LOGINDISABLED: %q", cmd)
		}
	}
}

func TestLoginInjectionRejectedBeforeWrite(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("NOOP")
		s.line(tag + " OK done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Login("u", "pass\nword"); !errors.Is(err, ErrInjection) {
		t.Fatalf("Login(CR-LF password) err = %v, want ErrInjection", err)
	}
	if err := c.Login("us\ner", "p"); !errors.Is(err, ErrInjection) {
		t.Fatalf("Login(CR-LF user) err = %v, want ErrInjection", err)
	}
	// Rejection happened before any write, so the client stays usable.
	if err := c.Noop(); err != nil {
		t.Errorf("Noop after injection refusal: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Errorf("Logout: %v", err)
	}
	wait()
	if got := strings.Join(s.cmds, "|"); got != "CAPABILITY|NOOP|LOGOUT" {
		t.Errorf("commands = %q, want no LOGIN anywhere", got)
	}
}

func TestLiteralBearingResponse(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("NOOP")
		// One untagged line carrying TWO chained literals, one of them with
		// embedded CRLF and parens — without literal support the reader
		// desyncs here.
		s.raw("* 23 FETCH (BODY[] {5}\r\n")
		s.raw("hel\r\n")
		s.raw("lo MORE {4}\r\n")
		s.raw("wo)r")
		s.raw("\r\n")
		s.line(tag + " OK noop done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Noop's exchange absorbs the literal-bearing untagged line; the tagged
	// OK still terminates the command.
	if err := c.Noop(); err != nil {
		t.Fatalf("Noop: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestCommandTimeout(t *testing.T) {
	t.Parallel()
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		if _, _, err := s.readCmd(); err != nil {
			return
		}
		// Never answer: the client's per-exchange deadline must fire. When
		// the client gives up it closes, which ends this read.
		_ = s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, _ = s.readCmd()
	})
	opts := dialOptions(dial, "none")
	opts.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := Dial(context.Background(), opts)
	if err == nil {
		t.Fatal("Dial against a silent server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Dial took %v, want ~100ms timeout", elapsed)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("err = %v, want a net timeout", err)
	}
	wait()
}

func TestMalformedResponseClosesConnection(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		if _, _, err := s.readCmd(); err != nil {
			return
		}
		s.line("GARBAGE NOT A RESPONSE")
		s.expectEOF()
	})
	_, err := Dial(context.Background(), dialOptions(dial, "none"))
	if err == nil {
		t.Fatal("Dial against a garbage response succeeded")
	}
	wait()
	if !s.SawEOF {
		t.Error("server did not see the connection close after a protocol error")
	}
}

func TestStartTLSUpgrade(t *testing.T) {
	t.Parallel()
	cert := selfSignedCert(t)
	dial, _, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1 STARTTLS LOGINDISABLED")
		s.line(tag + " OK done")
		tag = s.expectCmd("STARTTLS")
		s.line(tag + " OK begin TLS negotiation")
		s.upgradeTLS(cert)
		// RFC 2595: capabilities are re-fetched over TLS; LOGINDISABLED is
		// gone once the transport is private.
		tag = s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1 AUTH=PLAIN")
		s.line(tag + " OK done")
		tag = s.expectCmd(`LOGIN "u" "p"`)
		s.line(tag + " OK logged in")
		tag = s.expectCmd("LOGOUT")
		s.line("* BYE bye")
		s.line(tag + " OK done")
	})
	c, err := Dial(context.Background(), dialOptions(dial, "starttls"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if c.hasCap("LOGINDISABLED") {
		t.Error("post-TLS capabilities still carry LOGINDISABLED (not re-fetched?)")
	}
	if err := c.Login("u", "p"); err != nil {
		t.Fatalf("Login over TLS: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
}

func TestStartTLSNotAdvertised(t *testing.T) {
	t.Parallel()
	dial, s, wait := runFake(t, func(s *fakeSession) {
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		// STARTTLS was required but never advertised: the client errors and
		// hangs up.
		s.expectEOF()
	})
	_, err := Dial(context.Background(), dialOptions(dial, "starttls"))
	if err == nil {
		t.Fatal("Dial succeeded without a STARTTLS offer")
	}
	wait()
	if !s.SawEOF {
		t.Error("client did not close after STARTTLS was not advertised")
	}
}

func TestSSLImmediateTLS(t *testing.T) {
	t.Parallel()
	cert := selfSignedCert(t)
	dial, s, wait := runFake(t, func(s *fakeSession) {
		// ssl mode: TLS first, greeting inside it.
		s.upgradeTLS(cert)
		s.line("* OK ready")
		tag := s.expectCmd("CAPABILITY")
		s.line("* CAPABILITY IMAP4rev1")
		s.line(tag + " OK done")
		tag = s.expectCmd("LOGOUT")
		s.line(tag + " OK done")
	})
	c, err := Dial(context.Background(), dialOptions(dial, "ssl"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	wait()
	if !s.SawEOF && len(s.cmds) != 2 {
		t.Errorf("commands over TLS = %v", s.cmds)
	}
}

func TestDialOptionValidation(t *testing.T) {
	t.Parallel()
	if _, err := Dial(context.Background(), DialOptions{Host: "h", Port: 993, SSLMode: "none"}); err == nil {
		t.Error("Dial without DialContext succeeded")
	}
	pipe := func(context.Context, string, string) (net.Conn, error) {
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	if _, err := Dial(context.Background(), DialOptions{Port: 993, SSLMode: "none", DialContext: pipe}); err == nil {
		t.Error("Dial without Host succeeded")
	}
	if _, err := Dial(context.Background(), DialOptions{Host: "h", Port: 993, SSLMode: "auto", DialContext: pipe}); err == nil {
		t.Error("Dial with unknown ssl mode succeeded")
	}
	if _, err := Dial(context.Background(), DialOptions{Host: "h", Port: 70000, SSLMode: "none", DialContext: pipe}); err == nil {
		t.Error("Dial with out-of-range port succeeded")
	}
}

func TestReadLineLiterals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		wire string
		want string
	}{
		{"plain", "* OK hi\r\n", "* OK hi"},
		{"single literal", "* X {5}\r\nhello end\r\n", "* X {5}\r\nhello end"},
		{"chained literals", "* X {3}\r\nabc mid {4}\r\ndef) tail\r\n", "* X {3}\r\nabc mid {4}\r\ndef) tail"},
		{"literal with crlf inside", "* X {7}\r\na\r\nb\r\ncc\r\n", "* X {7}\r\na\r\nb\r\ncc"},
		{"zero literal", "* X {0}\r\n tail\r\n", "* X {0}\r\n tail"},
		{"trailing brace not a literal", "* X text}\r\n", "* X text}"},
		{"non-numeric braces", "* X {abc}\r\n", "* X {abc}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, client := net.Pipe()
			defer func() { _ = client.Close() }()
			go func() {
				_, _ = io.WriteString(server, tc.wire)
				_ = server.Close()
			}()
			c := &Client{conn: client, r: bufio.NewReader(client)}
			got, err := c.readLine()
			if err != nil {
				t.Fatalf("readLine: %v", err)
			}
			if got != tc.want {
				t.Errorf("readLine = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadLineOversizedLiteral(t *testing.T) {
	t.Parallel()
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		_, _ = io.WriteString(server, "* X {99999999}\r\n")
		_ = server.Close()
	}()
	c := &Client{conn: client, r: bufio.NewReader(client)}
	if _, err := c.readLine(); err == nil {
		t.Fatal("readLine accepted a literal above the cap")
	}
}

func TestQuoteString(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"simple", `"simple"`},
		{"with space", `"with space"`},
		{`quote"here`, `"quote\"here"`},
		{`back\slash`, `"back\\slash"`},
		{"", `""`},
	} {
		got, err := quoteString(tc.in)
		if err != nil {
			t.Fatalf("quoteString(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("quoteString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"a\nb", "a\rb", "\r\n", "a\r\nb"} {
		if _, err := quoteString(bad); !errors.Is(err, ErrInjection) {
			t.Errorf("quoteString(%q) err = %v, want ErrInjection", bad, err)
		}
	}
}

func TestLiteralOctets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		n    int
		ok   bool
	}{
		{"* OK hi\r\n", 0, false},
		{"* X {5}\r\n", 5, true},
		{"* X {5}\n", 5, true},
		{"* X {12+}\r\n", 12, true},
		{"* X {0}\r\n", 0, true},
		{"* X {abc}\r\n", 0, false},
		{"* X {}\r\n", 0, false},
		{"* X {5", 0, false},
		{"* X 5}\r\n", 0, false},
	} {
		n, ok := literalOctets([]byte(tc.line))
		if n != tc.n || ok != tc.ok {
			t.Errorf("literalOctets(%q) = %d,%v want %d,%v", tc.line, n, ok, tc.n, tc.ok)
		}
	}
}
