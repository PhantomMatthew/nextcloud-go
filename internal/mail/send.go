package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// send.go is the M5 send service (ADR-0108 §1): validate → compose → SMTP
// send over the guarded dialer → best-effort APPEND of the composed bytes
// to the Sent mailbox. No event publishes on send (Bus deliberately
// absent): arrival events are the sync engine's, and the APPEND is a local
// copy the next sync already knows about.

const (
	// maxOutgoingAttachments caps the attachment count of one send.
	maxOutgoingAttachments = 20
	// maxAttachmentSize caps one attachment's DECODED bytes (the handler's
	// files-path read uses it as the streaming cap too).
	maxAttachmentSize = 10 << 20
	// maxOutgoingTotal caps the SUM of decoded attachment bytes; the
	// composed-bytes cap (same 25 MiB) lives in compose.go.
	maxOutgoingTotal = 25 << 20
)

var (
	// ErrSendValidation reports a send request failing the shape checks: no
	// recipients, no body, or over the attachment count/size caps → 400.
	ErrSendValidation = errors.New("mail: invalid send request")
	// ErrSentAppend marks a best-effort save-to-Sent failure: the send
	// itself succeeded and its result stands. The OnAppendError hook and
	// the warn log observe the failure wrapped in this sentinel.
	ErrSentAppend = errors.New("mail: save to Sent failed")
)

// SendInput is one send request with every address already parsed (the
// handler validates via net/mail.ParseAddressList).
type SendInput struct {
	To, Cc, Bcc []mail.Address
	Subject     string
	BodyPlain   string
	BodyHTML    string
	InReplyTo   string
	References  string
	Attachments []OutgoingAttachment
}

// Sender runs the M5 send flow. Store is the mail Store (the Sent mailbox
// lookup); Secret opens the sealed credential pair; DialIMAP/DialSMTP are
// the dial seams (nil → package defaults, production wiring installs the
// egress-guarded closures); TLSConfig overrides the SMTP client TLS config
// (tests inject a self-signed trust — nil means verification on with
// ServerName=host); Logger is nil-safe. OnAppendError observes a
// best-effort Sent-append failure (nil-safe; a test hook — the send result
// never depends on it). Sender holds no per-request state.
type Sender struct {
	Store     Store
	Secret    string
	DialIMAP  func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error)
	DialSMTP  func(ctx context.Context, opts SMTPOptions) (net.Conn, error)
	TLSConfig *tls.Config
	Logger    *slog.Logger
	// OnAppendError is called with the ErrSentAppend-wrapped cause when the
	// best-effort save-to-Sent fails; the send result stands either way.
	OnAppendError func(error)
}

// Send validates in, composes the message, delivers it through the
// account's SMTP server, and best-effort saves it to the Sent mailbox. It
// returns the generated Message-ID.
func (s *Sender) Send(ctx context.Context, a *Account, in SendInput) (string, error) {
	if err := validateSend(in); err != nil {
		return "", err
	}
	imapPW, smtpPW, err := openPasswords(s.Secret, a.UserID, a.IMAPHost, a.IMAPUser, a.PasswordSealed)
	if err != nil {
		return "", err // a sealed blob that does not open is a loud 500
	}
	msg, messageID, err := compose(ComposeInput{
		From:        mail.Address{Name: a.Name, Address: a.Email},
		To:          in.To,
		Cc:          in.Cc,
		Bcc:         in.Bcc,
		Subject:     in.Subject,
		BodyPlain:   in.BodyPlain,
		BodyHTML:    in.BodyHTML,
		Attachments: in.Attachments,
		InReplyTo:   in.InReplyTo,
		References:  in.References,
	})
	if err != nil {
		return "", err
	}
	env := SendEnvelope{From: a.Email}
	for _, addrs := range [][]mail.Address{in.To, in.Cc, in.Bcc} {
		for _, addr := range addrs {
			env.Recipients = append(env.Recipients, addr.Address)
		}
	}
	if err := s.sendSMTP(ctx, a, smtpPW, msg, env); err != nil {
		return "", err
	}
	s.saveToSent(ctx, a, imapPW, msg)
	return messageID, nil
}

// validateSend enforces the send shape: at least one recipient across
// to/cc/bcc, at least one body, and the attachment count/size caps.
func validateSend(in SendInput) error {
	if len(in.To)+len(in.Cc)+len(in.Bcc) == 0 {
		return fmt.Errorf("%w: at least one recipient is required", ErrSendValidation)
	}
	if in.BodyPlain == "" && in.BodyHTML == "" {
		return fmt.Errorf("%w: a plain or HTML body is required", ErrSendValidation)
	}
	if len(in.Attachments) > maxOutgoingAttachments {
		return fmt.Errorf("%w: at most %d attachments per message", ErrSendValidation, maxOutgoingAttachments)
	}
	var total int64
	for _, at := range in.Attachments {
		if len(at.Data) > maxAttachmentSize {
			return fmt.Errorf("%w: attachment %q exceeds the 10 MiB limit", ErrSendValidation, at.Filename)
		}
		total += int64(len(at.Data))
	}
	if total > maxOutgoingTotal {
		return fmt.Errorf("%w: attachments exceed the 25 MiB total limit", ErrSendValidation)
	}
	return nil
}

// saveToSent best-effort APPENDs the composed bytes to the account's Sent
// mailbox (the synced row with special_use='sent') with \Seen. Every
// failure is logged and reported through OnAppendError; the send result
// stands either way.
func (s *Sender) saveToSent(ctx context.Context, a *Account, imapPW string, msg []byte) {
	fail := func(err error) {
		err = fmt.Errorf("%w: %w", ErrSentAppend, err)
		if s.Logger != nil {
			s.Logger.WarnContext(ctx, "mail: send: save to Sent failed", slog.String("error", err.Error()))
		}
		if s.OnAppendError != nil {
			s.OnAppendError(err)
		}
	}
	boxes, err := s.Store.ListMailboxes(ctx, a.ID)
	if err != nil {
		fail(fmt.Errorf("list mailboxes: %w", err))
		return
	}
	var sent *Mailbox
	for i := range boxes {
		if boxes[i].SpecialUse == "sent" {
			sent = &boxes[i]
			break
		}
	}
	if sent == nil {
		return // no synced Sent mailbox: nothing to save (not an error)
	}
	if err := s.appendToMailbox(ctx, a, imapPW, sent.Name, msg); err != nil {
		fail(err)
	}
}

// appendToMailbox runs one IMAP APPEND over a fresh session: dial → LOGIN →
// APPEND → LOGOUT — the ops.go openSession shape minus SELECT, which APPEND
// does not need.
func (s *Sender) appendToMailbox(ctx context.Context, a *Account, imapPW, wireName string, msg []byte) error {
	dial := s.DialIMAP
	if dial == nil {
		dial = imap.Dial
	}
	client, err := dial(ctx, imap.DialOptions{Host: a.IMAPHost, Port: a.IMAPPort, SSLMode: a.IMAPSSLMode})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	if err := client.Login(a.IMAPUser, imapPW); err != nil {
		return errors.Join(fmt.Errorf("login: %w", err), client.Logout())
	}
	defer func() {
		// LOGOUT is best-effort (ADR-0108 §1): a rude reply cannot un-append.
		if err := client.Logout(); err != nil && s.Logger != nil {
			s.Logger.DebugContext(ctx, "mail: send: sent-append logout failed", slog.String("error", err.Error()))
		}
	}()
	if err := client.Append(ctx, wireName, []string{`\Seen`}, time.Now(), msg); err != nil {
		return fmt.Errorf("append: %w", err)
	}
	return nil
}
