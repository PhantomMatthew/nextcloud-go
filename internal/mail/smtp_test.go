package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// smtpFake is a stateful scripted SMTP server on a loopback listener,
// injected into Sender.DialSMTP so send/handler tests exercise the REAL
// net/smtp client end to end — TLS included (self-signed cert, ssl mode
// wraps at accept, starttls upgrades on command). It records every command,
// the envelope, the AUTH credentials, and the DATA payload for assertions.
// The dial closure re-aims every dial at the listener; the account's ssl
// mode still drives the client side (the transport IS under test here).
type smtpFake struct {
	ln   net.Listener
	cert tls.Certificate
	wg   sync.WaitGroup

	mu sync.Mutex
	// knobs — construction-time ones pass through newSMTPFake options
	// (assigned before the accept loop spawns); anything changed AFTER the
	// first dial must go through the mu-guarded setters:
	tlsImmediate bool                         // ssl mode: TLS before the greeting
	starttls     bool                         // advertise STARTTLS
	auth         bool                         // advertise AUTH PLAIN
	acceptAuth   func(user, pass string) bool // nil → accept everything
	refuseRcpt   map[string]bool              // RCPT addresses answered 550
	// recording:
	cmds        []string
	mailFrom    string
	rcpts       []string
	data        []byte
	auths       [][2]string
	tlsSessions int
}

// newSMTPFake starts the fake; opts set knobs BEFORE the accept loop
// spawns (so no setter locking is needed for construction-time knobs).
func newSMTPFake(t *testing.T, opts ...func(*smtpFake)) *smtpFake {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &smtpFake{ln: ln, cert: smtpTestCert(t)}
	for _, opt := range opts {
		opt(f)
	}
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

// dialSMTP is the Sender.DialSMTP seam for this fake.
func (f *smtpFake) dialSMTP(ctx context.Context, _ SMTPOptions) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", f.ln.Addr().String())
}

// setAcceptAuth swaps the AUTH decision mid-test (mu-guarded — a serve
// goroutine may be reading it on another connection).
func (f *smtpFake) setAcceptAuth(fn func(user, pass string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acceptAuth = fn
}

// setRefuseRcpt swaps the refused-recipient set mid-test (mu-guarded).
func (f *smtpFake) setRefuseRcpt(m map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuseRcpt = m
}

// commands returns the recorded command lines (DATA payload excluded).
func (f *smtpFake) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *smtpFake) envelope() (string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mailFrom, append([]string(nil), f.rcpts...)
}

func (f *smtpFake) payload() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...)
}

func (f *smtpFake) authLog() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.auths...)
}

func (f *smtpFake) tlsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tlsSessions
}

func (f *smtpFake) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	f.mu.Lock()
	immediate := f.tlsImmediate
	cert := f.cert
	f.mu.Unlock()
	if immediate {
		tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(context.Background()); err != nil {
			return
		}
		conn = tc
		f.mu.Lock()
		f.tlsSessions++
		f.mu.Unlock()
	}
	r := bufio.NewReader(conn)
	_, _ = io.WriteString(conn, "220 fake ESMTP ready\r\n")
	tlsActive := immediate
	inData := false
	var data bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				f.mu.Lock()
				f.data = append([]byte(nil), data.Bytes()...)
				f.mu.Unlock()
				data.Reset()
				_, _ = io.WriteString(conn, "250 2.0.0 queued\r\n")
				continue
			}
			data.WriteString(strings.TrimPrefix(line, ".") + "\r\n") // dot-unstuffing
			continue
		}
		verb, args, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		f.mu.Lock()
		f.cmds = append(f.cmds, line)
		f.mu.Unlock()
		switch verb {
		case "EHLO", "HELO":
			f.mu.Lock()
			var caps []string
			if f.starttls && !tlsActive {
				caps = append(caps, "STARTTLS")
			}
			if f.auth {
				caps = append(caps, "AUTH PLAIN")
			}
			f.mu.Unlock()
			if len(caps) == 0 {
				_, _ = io.WriteString(conn, "250 fake greets you\r\n")
				break
			}
			var b strings.Builder
			b.WriteString("250-fake greets you\r\n")
			for i, c := range caps {
				sep := "-"
				if i == len(caps)-1 {
					sep = " "
				}
				b.WriteString("250" + sep + c + "\r\n")
			}
			_, _ = io.WriteString(conn, b.String())
		case "STARTTLS":
			_, _ = io.WriteString(conn, "220 2.0.0 ready to start TLS\r\n")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			if err := tc.HandshakeContext(context.Background()); err != nil {
				return
			}
			conn = tc
			r = bufio.NewReader(tc)
			tlsActive = true
			f.mu.Lock()
			f.tlsSessions++
			f.mu.Unlock()
		case "AUTH":
			// AUTH PLAIN <base64(identity \0 user \0 pass)>
			var user, pass string
			if parts := strings.Fields(args); len(parts) == 2 {
				if dec, err := base64.StdEncoding.DecodeString(parts[1]); err == nil {
					if segs := strings.SplitN(string(dec), "\x00", 3); len(segs) == 3 {
						user, pass = segs[1], segs[2]
					}
				}
			}
			f.mu.Lock()
			f.auths = append(f.auths, [2]string{user, pass})
			accept := f.acceptAuth == nil || f.acceptAuth(user, pass)
			f.mu.Unlock()
			if accept {
				_, _ = io.WriteString(conn, "235 2.7.0 authenticated\r\n")
			} else {
				_, _ = io.WriteString(conn, "535 5.7.8 authentication failed\r\n")
			}
		case "MAIL":
			f.mu.Lock()
			f.mailFrom = smtpAddrArg(args)
			f.mu.Unlock()
			_, _ = io.WriteString(conn, "250 2.1.0 sender ok\r\n")
		case "RCPT":
			addr := smtpAddrArg(args)
			f.mu.Lock()
			f.rcpts = append(f.rcpts, addr)
			refuse := f.refuseRcpt[addr]
			f.mu.Unlock()
			if refuse {
				_, _ = io.WriteString(conn, "550 5.1.1 no such user\r\n")
			} else {
				_, _ = io.WriteString(conn, "250 2.1.5 recipient ok\r\n")
			}
		case "DATA":
			_, _ = io.WriteString(conn, "354 end with <CR><LF>.<CR><LF>\r\n")
			inData = true
		case "QUIT":
			_, _ = io.WriteString(conn, "221 2.0.0 bye\r\n")
			return
		default:
			_, _ = io.WriteString(conn, "502 5.5.2 unsupported\r\n")
		}
	}
}

// smtpAddrArg extracts the address from "FROM:<a@b>" / "TO:<a@b>".
func smtpAddrArg(args string) string {
	open := strings.IndexByte(args, '<')
	closing := strings.IndexByte(args, '>')
	if open < 0 || closing < open {
		return args
	}
	return args[open+1 : closing]
}

// smtpTestCert builds a throwaway ECDSA certificate for the fake TLS server.
func smtpTestCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "smtp.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"smtp.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// --- the sendSMTP tests ---

// smtpTestSender builds a Sender whose only seams are the fake's dialer and
// a self-signed-trusting TLS config (test-only — the production path never
// disables verification).
func smtpTestSender(f *smtpFake) *Sender {
	return &Sender{
		DialSMTP:  f.dialSMTP,
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // test-only: the scripted fake is self-signed
	}
}

func smtpTestAccount(mode, user string) *Account {
	return &Account{Email: "alice@example.com", SMTPHost: "smtp.test", SMTPSSLMode: mode, SMTPUser: user}
}

var smtpTestMessage = []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Hi there\r\n\r\nbody text\r\n")

var smtpTestEnvelope = SendEnvelope{
	From:       "alice@example.com",
	Recipients: []string{"bob@example.com", "carol@example.com"}, // carol rides as the Bcc
}

// commandVerbs reduces the recorded command lines to their verbs.
func commandVerbs(cmds []string) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		verb, _, _ := strings.Cut(c, " ")
		out = append(out, verb)
	}
	return out
}

func TestSMTPSendStartTLS(t *testing.T) {
	t.Parallel()
	f := newSMTPFake(t, func(f *smtpFake) { f.starttls = true; f.auth = true })
	s := smtpTestSender(f)
	a := smtpTestAccount(SSLModeStartTLS, "alice@example.com")
	if err := s.sendSMTP(t.Context(), a, "smtp-secret-pw", smtpTestMessage, smtpTestEnvelope); err != nil {
		t.Fatalf("sendSMTP: %v", err)
	}
	if got := f.tlsCount(); got != 1 {
		t.Errorf("tls sessions = %d, want 1 (the STARTTLS upgrade)", got)
	}
	from, rcpts := f.envelope()
	if from != "alice@example.com" {
		t.Errorf("MAIL FROM = %q", from)
	}
	if strings.Join(rcpts, ",") != "bob@example.com,carol@example.com" {
		t.Errorf("RCPT set = %v — the Bcc recipient must reach RCPT", rcpts)
	}
	data := f.payload()
	if !bytes.Contains(data, []byte("Subject: Hi there")) {
		t.Errorf("DATA payload lacks the subject: %q", data)
	}
	if bytes.Contains(data, []byte("carol@example.com")) {
		t.Error("Bcc address leaked into the DATA bytes")
	}
	auths := f.authLog()
	if len(auths) != 1 || auths[0] != [2]string{"alice@example.com", "smtp-secret-pw"} {
		t.Errorf("auths = %v, want the account credentials once", auths)
	}
	wantVerbs := []string{"EHLO", "STARTTLS", "EHLO", "AUTH", "MAIL", "RCPT", "RCPT", "DATA", "QUIT"}
	if got := strings.Join(commandVerbs(f.commands()), ","); got != strings.Join(wantVerbs, ",") {
		t.Errorf("command sequence = %s, want %s", got, wantVerbs)
	}
}

func TestSMTPSendSSL(t *testing.T) {
	t.Parallel()
	f := newSMTPFake(t, func(f *smtpFake) { f.tlsImmediate = true; f.auth = true })
	s := smtpTestSender(f)
	a := smtpTestAccount(SSLModeSSL, "alice@example.com")
	if err := s.sendSMTP(t.Context(), a, "smtp-secret-pw", smtpTestMessage, smtpTestEnvelope); err != nil {
		t.Fatalf("sendSMTP: %v", err)
	}
	if got := f.tlsCount(); got != 1 {
		t.Errorf("tls sessions = %d, want 1 (TLS before the greeting)", got)
	}
	verbs := commandVerbs(f.commands())
	if len(verbs) == 0 || verbs[0] != "EHLO" {
		t.Errorf("first command = %v, want EHLO (TLS already up — no STARTTLS command)", verbs)
	}
	if auths := f.authLog(); len(auths) != 1 {
		t.Errorf("auths = %v, want 1", auths)
	}
}

// TestSMTPSendRelayNone covers relay mode: an empty SMTPUser skips AUTH
// entirely — no AUTH command may cross.
func TestSMTPSendRelayNone(t *testing.T) {
	t.Parallel()
	f := newSMTPFake(t)
	s := smtpTestSender(f)
	a := smtpTestAccount(SSLModeNone, "")
	if err := s.sendSMTP(t.Context(), a, "", smtpTestMessage, smtpTestEnvelope); err != nil {
		t.Fatalf("sendSMTP: %v", err)
	}
	for _, verb := range commandVerbs(f.commands()) {
		if verb == "AUTH" {
			t.Error("relay mode sent AUTH — an empty SMTPUser must skip AUTH")
		}
	}
	if got := f.tlsCount(); got != 0 {
		t.Errorf("tls sessions = %d, want 0 (plaintext relay)", got)
	}
}

// TestSMTPCleartextAuthRefused pins the fail-closed rule: credentials over
// an effective-cleartext connection (mode none, no STARTTLS) are refused
// BEFORE anything auth-related crosses — the fake sees the greeting and
// nothing else.
func TestSMTPCleartextAuthRefused(t *testing.T) {
	t.Parallel()
	f := newSMTPFake(t)
	s := smtpTestSender(f)
	a := smtpTestAccount(SSLModeNone, "alice@example.com")
	err := s.sendSMTP(t.Context(), a, "smtp-secret-pw", smtpTestMessage, smtpTestEnvelope)
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "cleartext authentication refused") {
		t.Fatalf("err = %v, want ErrUpstream wrapping the cleartext refusal", err)
	}
	if cmds := f.commands(); len(cmds) != 0 {
		t.Errorf("commands after the refusal = %v, want none (not even EHLO)", cmds)
	}
}

func TestSMTPStartTLSNotAdvertised(t *testing.T) {
	t.Parallel()
	f := newSMTPFake(t) // no STARTTLS capability
	s := smtpTestSender(f)
	a := smtpTestAccount(SSLModeStartTLS, "alice@example.com")
	err := s.sendSMTP(t.Context(), a, "smtp-secret-pw", smtpTestMessage, smtpTestEnvelope)
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want ErrUpstream for the missing STARTTLS", err)
	}
	for _, verb := range commandVerbs(f.commands()) {
		if verb == "STARTTLS" || verb == "AUTH" || verb == "MAIL" {
			t.Errorf("command %q crossed after the capability check failed", verb)
		}
	}
}

func TestSMTPAuthFailure(t *testing.T) {
	t.Parallel()
	f := newSMTPFake(t, func(f *smtpFake) {
		f.starttls = true
		f.auth = true
		f.acceptAuth = func(_, _ string) bool { return false }
	})
	s := smtpTestSender(f)
	a := smtpTestAccount(SSLModeStartTLS, "alice@example.com")
	err := s.sendSMTP(t.Context(), a, "wrong-pw", smtpTestMessage, smtpTestEnvelope)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream (502 class)", err)
	}
	for _, verb := range commandVerbs(f.commands()) {
		if verb == "MAIL" || verb == "DATA" {
			t.Errorf("command %q crossed after the auth failure", verb)
		}
	}
}

func TestSMTPRecipientRefused(t *testing.T) {
	t.Parallel()
	f := newSMTPFake(t, func(f *smtpFake) { f.refuseRcpt = map[string]bool{"carol@example.com": true} })
	s := smtpTestSender(f)
	a := smtpTestAccount(SSLModeNone, "")
	err := s.sendSMTP(t.Context(), a, "", smtpTestMessage, smtpTestEnvelope)
	if !errors.Is(err, ErrRecipientRefused) {
		t.Fatalf("err = %v, want ErrRecipientRefused", err)
	}
	if !strings.Contains(err.Error(), "carol@example.com") {
		t.Errorf("error %q must name the refused address", err)
	}
	_, rcpts := f.envelope()
	if strings.Join(rcpts, ",") != "bob@example.com,carol@example.com" {
		t.Errorf("RCPT attempts = %v, want both (the refusal aborts after carol)", rcpts)
	}
	for _, verb := range commandVerbs(f.commands()) {
		if verb == "DATA" {
			t.Error("DATA crossed after an RCPT refusal")
		}
	}
}

func TestSMTPDialFailure(t *testing.T) {
	t.Parallel()
	s := &Sender{DialSMTP: func(context.Context, SMTPOptions) (net.Conn, error) {
		return nil, errors.New("no route")
	}}
	a := smtpTestAccount(SSLModeSSL, "")
	err := s.sendSMTP(t.Context(), a, "", smtpTestMessage, smtpTestEnvelope)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
}
