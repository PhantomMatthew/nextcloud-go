package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"time"
)

// smtp.go is the M5 SMTP send half (ADR-0108 §1): stdlib net/smtp over the
// egress-guarded dialer, in package mail (no subpackage — the seam is a
// field on Sender, not a DialOptions struct like imap's). TLS modes mirror
// the IMAP side: "ssl" wraps before the greeting, "starttls" upgrades after
// EHLO (the server must advertise it), "none" is plaintext — and plaintext
// with credentials is refused before AUTH, because AUTH PLAIN over
// cleartext would expose the password.

// smtpSessionTimeout bounds one whole send session (greeting → QUIT): the
// DATA phase of a 25 MiB message dominates, so the budget is generous.
const smtpSessionTimeout = 5 * time.Minute

// ErrRecipientRefused reports an RCPT TO the server answered with an error
// → 400; the wrapped message names the refused address.
var ErrRecipientRefused = errors.New("mail: recipient refused by the server")

// SMTPOptions is the DialSMTP seam input: the connection target resolved
// from the account (Port already carries the mode default — the production
// closure only has to build the address and dial through the egress guard).
type SMTPOptions struct {
	Host    string
	Port    int
	SSLMode string // "ssl" | "starttls" | "none"
}

// SendEnvelope is the SMTP envelope: Bcc recipients appear here and ONLY
// here — never in the composed message bytes.
type SendEnvelope struct {
	From       string
	Recipients []string
}

// sendSMTP delivers msg over one SMTP session: dial through the seam → TLS
// per mode → AUTH (unless relay mode) → MAIL/RCPT/DATA → QUIT. Error
// mapping: dial/TLS/transport/auth/DATA → ErrUpstream (502 class); an RCPT
// refusal → ErrRecipientRefused naming the address.
func (s *Sender) sendSMTP(ctx context.Context, a *Account, smtpPW string, msg []byte, env SendEnvelope) error {
	dial := s.DialSMTP
	if dial == nil {
		dial = defaultDialSMTP
	}
	mode := a.SMTPSSLMode
	if mode == "" {
		mode = SSLModeSSL
	}
	port := a.SMTPPort
	if port == 0 {
		switch mode {
		case SSLModeStartTLS:
			port = 587
		case SSLModeNone:
			port = 25
		default:
			port = 465
		}
	}
	conn, err := dial(ctx, SMTPOptions{Host: a.SMTPHost, Port: port, SSLMode: mode})
	if err != nil {
		return fmt.Errorf("%w: dial: %w", ErrUpstream, err)
	}
	if err := conn.SetDeadline(time.Now().Add(smtpSessionTimeout)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("%w: set deadline: %w", ErrUpstream, err)
	}
	tlsCfg := s.smtpTLSConfig(a.SMTPHost)
	if mode == SSLModeSSL {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("%w: TLS handshake: %w", ErrUpstream, err)
		}
		conn = tc
	}
	client, err := smtp.NewClient(conn, a.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("%w: greeting: %w", ErrUpstream, err)
	}
	defer func() { _ = client.Close() }()
	switch mode {
	case SSLModeStartTLS:
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%w: server does not advertise STARTTLS", ErrUpstream)
		}
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("%w: STARTTLS: %w", ErrUpstream, err)
		}
	case SSLModeSSL, SSLModeNone:
	default:
		return fmt.Errorf("%w: unknown ssl mode %q", ErrUpstream, mode)
	}
	if a.SMTPUser != "" {
		if mode == SSLModeNone {
			// Effective cleartext (no TLS, no STARTTLS): AUTH PLAIN would
			// expose the password on the wire. Refuse BEFORE anything auth-
			// related is sent — stdlib PlainAuth's own non-TLS refusal is
			// only the second line.
			return fmt.Errorf("%w: cleartext authentication refused", ErrUpstream)
		}
		if err := client.Auth(smtp.PlainAuth("", a.SMTPUser, smtpPW, a.SMTPHost)); err != nil {
			return fmt.Errorf("%w: authentication: %w", ErrUpstream, err)
		}
	}
	if err := client.Mail(env.From); err != nil {
		return fmt.Errorf("%w: MAIL FROM: %w", ErrUpstream, err)
	}
	for _, rcpt := range env.Recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrRecipientRefused, rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("%w: DATA: %w", ErrUpstream, err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("%w: write message: %w", ErrUpstream, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%w: message body refused: %w", ErrUpstream, err)
	}
	if err := client.Quit(); err != nil && s.Logger != nil {
		// QUIT is best-effort: DATA's 250 already accepted the message, so a
		// rude reply cannot un-send it (the imap Logout convention).
		s.Logger.DebugContext(ctx, "mail: smtp: quit failed after a successful send", slog.String("error", err.Error()))
	}
	return nil
}

// smtpTLSConfig renders the client TLS config: verification is always on
// unless a test injected a TLSConfig that says otherwise — the zero-value
// path never sets InsecureSkipVerify (the imap.Dial convention).
func (s *Sender) smtpTLSConfig(host string) *tls.Config {
	if s.TLSConfig == nil {
		return &tls.Config{ServerName: host}
	}
	cfg := s.TLSConfig.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	return cfg
}

// defaultDialSMTP is the nil-seam fallback (the imap.Dial precedent):
// production wiring ALWAYS installs the egress-guarded closure on
// Sender.DialSMTP, so this default only serves bare constructions.
func defaultDialSMTP(ctx context.Context, opts SMTPOptions) (net.Conn, error) {
	return (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext(ctx, "tcp", net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)))
}
