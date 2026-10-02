// Package imap implements a minimal IMAP4rev1 (RFC 3501) client for the
// Mail epic (ADR-0108 §1): greeting, CAPABILITY, LOGIN, LOGOUT, NOOP, and
// STARTTLS upgrade, plus the M3 sync commands (mailbox.go): LIST, EXAMINE
// (read-only), UID SEARCH, and UID FETCH summary/flags shapes. The response
// reader is literal-capable and the token parser (parse.go) handles atoms,
// quoted strings, parenthesized lists, NIL, and literals, which the
// ENVELOPE parsing builds on; RFC 2152 mailbox-name decoding lives in
// mutf7.go. Message-body FETCH (BODY/BODYSTRUCTURE) is M4 scope.
//
// The client carries no egress policy: DialOptions.DialContext is injected
// by the caller, and the app wiring installs the ADR-0057 guarded dialer
// (internal/netx) there.
package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds each command exchange (and the greeting/TLS
// handshakes) when DialOptions.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// maxLiteralSize caps one RFC 3501 literal so a hostile server cannot force
// an unbounded allocation with a {999999999} marker.
const maxLiteralSize = 32 << 20

var (
	// ErrAuthentication reports a LOGIN answered with tagged NO: the server
	// rejected the credentials. Callers classify with errors.Is.
	ErrAuthentication = errors.New("imap: authentication failed")
	// ErrInjection reports a command argument containing CR or LF, which
	// would break out of the command line; quoteString rejects it before
	// anything is written.
	ErrInjection = errors.New("imap: argument contains CR or LF")
	// ErrLoginDisabled reports a LOGIN attempt against a server advertising
	// LOGINDISABLED (plaintext mode without STARTTLS): the client fails
	// closed instead of sending credentials the server will refuse.
	ErrLoginDisabled = errors.New("imap: LOGIN disabled by server (LOGINDISABLED)")
	// errBroken is returned by every command method after a fatal protocol
	// or transport error: the stream state is unknowable, so the client is
	// permanently unusable (only Logout stays callable, as a no-op).
	errBroken = errors.New("imap: connection unusable after a fatal error")
)

// DialOptions configures Dial. DialContext is REQUIRED — production wiring
// installs the egress-guarded dialer (internal/netx.GuardedDialContext);
// tests inject net.Pipe or loopback dialers.
type DialOptions struct {
	Host        string
	Port        int    // 0 selects 993 (ssl) or 143 (starttls/none)
	SSLMode     string // "ssl" | "starttls" | "none" (mail.SSLMode* values)
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	TLSConfig   *tls.Config   // nil → &tls.Config{ServerName: Host}; tests may inject (self-signed)
	Timeout     time.Duration // 0 → DefaultTimeout
}

// Client is one IMAP connection: a net.Conn plus buffered reader, speaking
// tagged commands (a1, a2, ...). A command completes on its tagged
// response; untagged lines received meanwhile are collected. After a fatal
// protocol or transport error the client is broken and every command
// method errors — the connection is closed, never reused.
type Client struct {
	conn    net.Conn
	r       *bufio.Reader
	host    string
	tlsCfg  *tls.Config
	timeout time.Duration
	tagSeq  int
	caps    []string
	broken  bool
}

// taggedResponse is the outcome of one command exchange.
type taggedResponse struct {
	status   string   // OK, NO, or BAD
	text     string   // the rest of the tagged line
	untagged []string // complete untagged lines received during the exchange
}

// Dial connects, negotiates TLS per SSLMode, reads the greeting, and
// fetches capabilities. "* OK" and "* PREAUTH" greetings are both ready
// (M2 treats PREAUTH as ready, ADR-0108 §1); "* BYE" is an error. For
// "starttls" the server must advertise STARTTLS: the connection upgrades
// after the greeting and capabilities are re-fetched over TLS (RFC 2595).
// TLS verification is always on unless the caller injects a TLSConfig that
// says otherwise — the zero-value path never sets InsecureSkipVerify. Any
// failure closes the connection.
func Dial(ctx context.Context, opts DialOptions) (*Client, error) {
	if opts.DialContext == nil {
		return nil, errors.New("imap: DialOptions.DialContext is required")
	}
	if opts.Host == "" {
		return nil, errors.New("imap: DialOptions.Host is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	port := opts.Port
	if port == 0 {
		if opts.SSLMode == "" || opts.SSLMode == "ssl" {
			port = 993
		} else {
			port = 143
		}
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("imap: invalid port %d", opts.Port)
	}
	tlsCfg := opts.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: opts.Host}
	} else {
		tlsCfg = tlsCfg.Clone()
		if tlsCfg.ServerName == "" {
			tlsCfg.ServerName = opts.Host
		}
	}
	address := net.JoinHostPort(opts.Host, strconv.Itoa(port))
	conn, err := opts.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", address, err)
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn), host: opts.Host, tlsCfg: tlsCfg, timeout: timeout}
	fail := func(err error) (*Client, error) {
		_ = conn.Close()
		return nil, err
	}
	switch opts.SSLMode {
	case "", "ssl":
		if err := c.wrapTLS(ctx); err != nil {
			return fail(err)
		}
	case "starttls", "none":
	default:
		return fail(fmt.Errorf("imap: unknown ssl mode %q", opts.SSLMode))
	}
	if err := c.readGreeting(); err != nil {
		return fail(err)
	}
	// One explicit CAPABILITY exchange for every mode keeps a single code
	// path (greeting capability codes are deliberately not parsed).
	if _, err := c.Capability(); err != nil {
		return fail(err)
	}
	if opts.SSLMode == "starttls" {
		if !c.hasCap("STARTTLS") {
			return fail(errors.New("imap: server does not advertise STARTTLS"))
		}
		resp, err := c.roundTrip("STARTTLS")
		if err != nil {
			return fail(err)
		}
		if resp.status != "OK" {
			return fail(fmt.Errorf("imap: STARTTLS refused: %s %s", resp.status, resp.text))
		}
		if err := c.wrapTLS(ctx); err != nil {
			return fail(err)
		}
		// RFC 2595: capabilities MAY differ once TLS is up; re-fetch.
		if _, err := c.Capability(); err != nil {
			return fail(err)
		}
	}
	return c, nil
}

// readGreeting consumes the server's first line: "* OK" (ready),
// "* PREAUTH" (ready, already authenticated), "* BYE" (refused).
func (c *Client) readGreeting() error {
	if err := c.setDeadline(); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return fmt.Errorf("imap: read greeting: %w", err)
	}
	switch {
	case strings.HasPrefix(line, "* OK"), strings.HasPrefix(line, "* PREAUTH"):
		return nil
	case strings.HasPrefix(line, "* BYE"):
		return fmt.Errorf("imap: server refused the connection: %s", line)
	default:
		return fmt.Errorf("imap: malformed greeting %q", line)
	}
}

// Capability returns the server's capability list, upper-cased, and
// refreshes the client's cached set (LOGIN policy, STARTTLS detection).
func (c *Client) Capability() ([]string, error) {
	resp, err := c.roundTrip("CAPABILITY")
	if err != nil {
		return nil, err
	}
	if resp.status != "OK" {
		return nil, c.fail(fmt.Errorf("imap: CAPABILITY: %s %s", resp.status, resp.text))
	}
	caps := []string{}
	for _, line := range resp.untagged {
		if line != "* CAPABILITY" && !strings.HasPrefix(line, "* CAPABILITY ") {
			continue
		}
		for _, tok := range strings.Fields(strings.TrimPrefix(line, "* CAPABILITY")) {
			caps = append(caps, strings.ToUpper(tok))
		}
	}
	c.caps = caps
	return append([]string(nil), caps...), nil
}

// Login authenticates with the LOGIN command (credentials sent as quoted
// strings through quoteString, so CR/LF injection is impossible). A server
// advertising LOGINDISABLED fails closed with ErrLoginDisabled before
// anything is written. Tagged NO maps to ErrAuthentication; BAD (or any
// exchange failure) is a fatal protocol error. Any failure closes the
// connection (ADR-0108 §1).
func (c *Client) Login(user, pass string) error {
	if c.broken {
		return errBroken
	}
	if c.hasCap("LOGINDISABLED") {
		return fmt.Errorf("%w: credentials would be sent in the clear", ErrLoginDisabled)
	}
	u, err := quoteString(user)
	if err != nil {
		return err
	}
	p, err := quoteString(pass)
	if err != nil {
		return err
	}
	resp, err := c.roundTrip("LOGIN " + u + " " + p)
	if err != nil {
		return err
	}
	switch resp.status {
	case "OK":
		return nil
	case "NO":
		return c.fail(withText(ErrAuthentication, resp.text))
	default:
		return c.fail(fmt.Errorf("imap: LOGIN rejected (%s): %s", resp.status, resp.text))
	}
}

// Noop is the RFC 3501 keep-alive: any untagged news arrives in its
// exchange and is discarded (M2 has no mailbox state to update).
func (c *Client) Noop() error {
	resp, err := c.roundTrip("NOOP")
	if err != nil {
		return err
	}
	if resp.status != "OK" {
		return c.fail(fmt.Errorf("imap: NOOP: %s %s", resp.status, resp.text))
	}
	return nil
}

// Logout sends LOGOUT and closes the connection. It is best-effort: the
// customary "* BYE" then a hung-up peer, and any other transport failure,
// are tolerated (nil); only a tagged NO/BAD reply surfaces as an error. It
// is idempotent — safe to call on an already-broken or closed client.
func (c *Client) Logout() error {
	if c.broken {
		return nil
	}
	defer func() {
		c.broken = true
		_ = c.conn.Close()
	}()
	resp, err := c.roundTrip("LOGOUT")
	if err != nil {
		return nil
	}
	if resp.status != "OK" {
		return fmt.Errorf("imap: LOGOUT: %s %s", resp.status, resp.text)
	}
	return nil
}

// hasCap reports whether the cached (upper-cased) capability set holds name.
func (c *Client) hasCap(name string) bool {
	for _, cp := range c.caps {
		if cp == name {
			return true
		}
	}
	return false
}

// roundTrip writes one tagged command and reads to its tagged response,
// collecting untagged lines on the way. The per-exchange I/O deadline is
// armed before the write and cleared on success so an idle-but-healthy
// connection is never killed between commands.
func (c *Client) roundTrip(cmd string) (taggedResponse, error) {
	if c.broken {
		return taggedResponse{}, errBroken
	}
	if err := c.setDeadline(); err != nil {
		return taggedResponse{}, c.fail(err)
	}
	c.tagSeq++
	tag := "a" + strconv.Itoa(c.tagSeq)
	if _, err := io.WriteString(c.conn, tag+" "+cmd+"\r\n"); err != nil {
		return taggedResponse{}, c.fail(fmt.Errorf("imap: write command: %w", err))
	}
	var resp taggedResponse
	for {
		line, err := c.readLine()
		if err != nil {
			return taggedResponse{}, c.fail(fmt.Errorf("imap: read response: %w", err))
		}
		switch {
		case strings.HasPrefix(line, "* "):
			resp.untagged = append(resp.untagged, line)
		case strings.HasPrefix(line, "+ "):
			// M2 commands never legitimately receive a continuation (no
			// literal-bearing commands are sent), so one means the server
			// and client disagree about the exchange: fatal.
			return taggedResponse{}, c.fail(fmt.Errorf("imap: unexpected continuation %q", line))
		case strings.HasPrefix(line, tag+" "):
			status, text, _ := strings.Cut(strings.TrimPrefix(line, tag+" "), " ")
			switch status {
			case "OK", "NO", "BAD":
				resp.status, resp.text = status, text
				if err := c.conn.SetDeadline(time.Time{}); err != nil {
					return taggedResponse{}, c.fail(err)
				}
				return resp, nil
			default:
				return taggedResponse{}, c.fail(fmt.Errorf("imap: malformed tagged status in %q", line))
			}
		default:
			return taggedResponse{}, c.fail(fmt.Errorf("imap: malformed response line %q", line))
		}
	}
}

// readLine reads one logical response line. When a fragment terminates with
// an RFC 3501 literal marker "{n}" exactly n more bytes are absorbed and
// assembly continues — literals can chain, and without this the stream
// desyncs on the first literal. The returned line keeps the literal markers
// and bytes (parse.go's tokenizer consumes them) and drops only the final
// CRLF.
func (c *Client) readLine() (string, error) {
	var buf []byte
	for {
		frag, err := c.r.ReadBytes('\n')
		buf = append(buf, frag...)
		if err != nil {
			return "", err
		}
		n, ok := literalOctets(buf)
		if !ok {
			line := strings.TrimSuffix(string(buf), "\n")
			return strings.TrimSuffix(line, "\r"), nil
		}
		if n > maxLiteralSize {
			return "", fmt.Errorf("imap: literal of %d octets exceeds the %d limit", n, maxLiteralSize)
		}
		lit := make([]byte, n)
		if _, err := io.ReadFull(c.r, lit); err != nil {
			return "", err
		}
		buf = append(buf, lit...)
	}
}

// literalOctets reports whether line terminates with a literal marker
// ("{n}\r\n", tolerating the RFC 2088 "{n+}" form), and if so how many
// octets follow. A trailing "{...}" that is not decimal digits is not a
// marker and ends the line normally.
func literalOctets(line []byte) (int, bool) {
	l := strings.TrimSuffix(string(line), "\n")
	l = strings.TrimSuffix(l, "\r")
	if l == "" || l[len(l)-1] != '}' {
		return 0, false
	}
	open := strings.LastIndexByte(l, '{')
	if open < 0 {
		return 0, false
	}
	digits := strings.TrimSuffix(l[open+1:len(l)-1], "+")
	if digits == "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// wrapTLS upgrades the connection to TLS (verification per tlsCfg, which
// always carries ServerName) and re-buffers the reader.
func (c *Client) wrapTLS(ctx context.Context) error {
	if err := c.setDeadline(); err != nil {
		return err
	}
	tc := tls.Client(c.conn, c.tlsCfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("imap: TLS handshake with %s: %w", c.host, err)
	}
	c.conn = tc
	c.r = bufio.NewReader(tc)
	return nil
}

func (c *Client) setDeadline() error {
	if err := c.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return fmt.Errorf("imap: set deadline: %w", err)
	}
	return nil
}

// fail marks the client permanently unusable and closes the connection:
// after a fatal protocol or transport error the stream state is unknowable,
// so the client fails closed.
func (c *Client) fail(err error) error {
	c.broken = true
	_ = c.conn.Close()
	return err
}

// quoteString renders s as an IMAP quoted string with \ and " escaped. Any
// CR or LF is rejected with ErrInjection — LOGIN credentials and every
// future string argument pass through here so a crafted value can never
// break out of the command line.
func quoteString(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n") {
		return "", ErrInjection
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '"':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String(), nil
}

// withText wraps sentinel with the server's response text when present.
func withText(sentinel error, text string) error {
	if text == "" {
		return sentinel
	}
	return fmt.Errorf("%w: %s", sentinel, text)
}
