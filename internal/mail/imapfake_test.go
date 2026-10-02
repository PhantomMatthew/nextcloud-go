package mail

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// imapFake is a scripted IMAP server on a loopback listener, injected into
// Service.DialIMAP so handler/service tests exercise the REAL imap.Client
// end to end. The dial closure it hands out forces ssl mode "none" and
// re-aims every dial at the listener — the semantics under test are the
// greeting/CAPABILITY/LOGIN/LOGOUT exchange, not TLS (the imap package's
// own tests cover transports). accept decides whether a LOGIN succeeds.
type imapFake struct {
	ln     net.Listener
	accept func(user, pass string) bool
	dials  atomic.Int32
	logins atomic.Int32
	wg     sync.WaitGroup
}

func newIMAPFake(t *testing.T, accept func(user, pass string) bool) *imapFake {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &imapFake{ln: ln, accept: accept}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.wg.Wait()
	})
	return f
}

// dialIMAP is the Service.DialIMAP seam for this fake.
func (f *imapFake) dialIMAP(ctx context.Context, opts imap.DialOptions) (*imap.Client, error) {
	f.dials.Add(1)
	opts.SSLMode = SSLModeNone
	opts.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, f.ln.Addr().String())
	}
	return imap.Dial(ctx, opts)
}

// serve speaks just enough IMAP4rev1 for imap.Dial + Login + Logout.
func (f *imapFake) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	_, _ = io.WriteString(conn, "* OK fake IMAP4rev1 ready\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		verb, args, _ := strings.Cut(rest, " ")
		switch verb {
		case "CAPABILITY":
			_, _ = io.WriteString(conn, "* CAPABILITY IMAP4rev1\r\n"+tag+" OK capability done\r\n")
		case "LOGIN":
			f.logins.Add(1)
			user, pass := splitLoginArgs(args)
			if f.accept(user, pass) {
				_, _ = io.WriteString(conn, tag+" OK logged in\r\n")
			} else {
				_, _ = io.WriteString(conn, tag+" NO [AUTHENTICATIONFAILED] invalid credentials\r\n")
			}
		case "LOGOUT":
			_, _ = io.WriteString(conn, "* BYE bye\r\n"+tag+" OK logout done\r\n")
			return
		default:
			_, _ = io.WriteString(conn, tag+" BAD unsupported\r\n")
		}
	}
}

// splitLoginArgs unpacks the two quoted arguments of `LOGIN "u" "p"` (test
// credentials never carry escapes).
func splitLoginArgs(args string) (user, pass string) {
	var out []string
	rest := args
	for len(out) < 2 {
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "\"") {
			break
		}
		end := strings.Index(rest[1:], "\"")
		if end < 0 {
			break
		}
		out = append(out, rest[1:1+end])
		rest = rest[1+end+1:]
	}
	if len(out) == 2 {
		return out[0], out[1]
	}
	return "", ""
}
