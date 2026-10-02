package imap

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSession is the server side of one scripted IMAP conversation. Tests
// drive it with a script func; every command the client sends is recorded
// in cmds (tag stripped) so scripts and tests can assert the exact command
// sequence (e.g. that LOGIN never arrived).
type fakeSession struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	cmds []string
	// SawEOF is set when a read after the client's last command hits EOF —
	// the observable proof the client closed the connection.
	SawEOF bool
}

func (s *fakeSession) line(l string) {
	s.t.Helper()
	if _, err := io.WriteString(s.conn, l+"\r\n"); err != nil {
		s.t.Errorf("fake: write %q: %v", l, err)
	}
}

// raw writes bytes verbatim (literal payloads).
func (s *fakeSession) raw(b string) {
	s.t.Helper()
	if _, err := io.WriteString(s.conn, b); err != nil {
		s.t.Errorf("fake: write raw: %v", err)
	}
}

// readCmd reads one command line and returns its tag and the rest
// (verb + arguments). The rest is appended to cmds.
func (s *fakeSession) readCmd() (string, string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
	s.cmds = append(s.cmds, rest)
	return tag, rest, nil
}

// expectCmd reads one command and requires it to be exactly want.
func (s *fakeSession) expectCmd(want string) string {
	s.t.Helper()
	tag, rest, err := s.readCmd()
	if err != nil {
		s.t.Errorf("fake: read command (want %q): %v", want, err)
		return ""
	}
	if rest != want {
		s.t.Errorf("fake: command = %q, want %q", rest, want)
	}
	return tag
}

// expectEOF requires the client to have hung up.
func (s *fakeSession) expectEOF() {
	s.t.Helper()
	if _, _, err := s.readCmd(); err == nil {
		s.t.Errorf("fake: expected EOF, got another command")
		return
	}
	s.SawEOF = true
}

// upgradeTLS wraps the server side in TLS (STARTTLS tests) and re-buffers
// the reader; the handshake runs lazily on the first TLS read.
func (s *fakeSession) upgradeTLS(cert tls.Certificate) {
	s.t.Helper()
	tc := tls.Server(s.conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(context.Background()); err != nil {
		s.t.Errorf("fake: TLS handshake: %v", err)
		return
	}
	s.conn = tc
	s.r = bufio.NewReader(tc)
}

// runFake starts script on one end of a net.Pipe and returns a DialContext
// yielding the client end, plus a wait func bounding how long the test
// blocks for the script to return.
func runFake(t *testing.T, script func(s *fakeSession)) (func(ctx context.Context, network, address string) (net.Conn, error), *fakeSession, func()) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	s := &fakeSession{t: t, conn: serverConn, r: bufio.NewReader(serverConn)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = serverConn.Close() }()
		script(s)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("fake script still running at cleanup")
		}
	})
	dial := func(context.Context, string, string) (net.Conn, error) { return clientConn, nil }
	wait := func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("fake script did not finish")
		}
	}
	return dial, s, wait
}

// dialOptions returns DialOptions wired to the fake over the given ssl
// mode, with TLS verification satisfied by the injected test config.
func dialOptions(dial func(ctx context.Context, network, address string) (net.Conn, error), sslMode string) DialOptions {
	return DialOptions{
		Host:        "imap.test",
		Port:        993,
		SSLMode:     sslMode,
		DialContext: dial,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true}, // test-only: the scripted pipe server is self-signed
		Timeout:     2 * time.Second,
	}
}

// selfSignedCert builds a throwaway ECDSA certificate for the fake TLS
// server.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "imap.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"imap.test"},
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
