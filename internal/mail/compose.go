package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"time"
)

// compose.go is the M5 MIME composer (ADR-0108 §1): it renders the full
// RFC 822 message for one send. Stdlib only. Bcc NEVER appears in the
// message bytes — it is an envelope-only (RCPT) concept. The body tree is
// text/plain alone, multipart/alternative (plain+html), or multipart/mixed
// wrapping the body part when attachments ride along; text leaves are
// charset=utf-8 quoted-printable, attachments base64 (76-column wraps) with
// an RFC 2231 filename via mime.FormatMediaType.

// maxComposedMessageSize caps the total composed message (headers + body +
// base64 attachments) at 25 MiB — the same ceiling the IMAP APPEND literal
// and the Sender's decoded-attachment total use.
const maxComposedMessageSize = 25 << 20

// ErrCompose reports an input that cannot become a valid message: no
// body, a CR/LF in a subject/display name/filename (header injection),
// or a threading header that is not one <message-id>. The handler maps
// it to 400.
var ErrCompose = errors.New("mail: cannot compose message")

// messageIDRe is the strict shape In-Reply-To/References must already have
// (the M3 sync stores message-ids with the angle brackets stripped — the
// handler re-wraps before calling Compose).
var messageIDRe = regexp.MustCompile(`^<[^<>\r\n]+>$`)

// ComposeInput is everything one outgoing message needs. The addresses
// arrive PRE-PARSED (the handler validates via net/mail.ParseAddressList,
// which already rejects CR/LF); From carries the account email and display
// name.
type ComposeInput struct {
	From        mail.Address
	To, Cc, Bcc []mail.Address
	// Subject is RFC 2047 Q-encoded (mime.QEncoding) when non-ASCII.
	Subject string
	// At least one of BodyPlain/BodyHTML is required (ErrCompose otherwise).
	BodyPlain, BodyHTML string
	Attachments         []OutgoingAttachment
	// InReplyTo/References are optional threading headers; when non-empty
	// each must match ^<[^<>\r\n]+>$.
	InReplyTo, References string
}

// OutgoingAttachment is one attachment's decoded content plus its metadata.
type OutgoingAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Compose renders the message and enforces the 25 MiB total cap
// (ErrMessageTooLarge on overflow).
func Compose(in ComposeInput) ([]byte, error) {
	msg, _, err := compose(in)
	return msg, err
}

// compose is Compose plus the generated Message-ID, which Sender.Send needs
// for its 200 {messageId} response.
func compose(in ComposeInput) ([]byte, string, error) {
	if in.BodyPlain == "" && in.BodyHTML == "" {
		return nil, "", fmt.Errorf("%w: a plain or HTML body is required", ErrCompose)
	}
	domain, ok := addressDomain(in.From.Address)
	if !ok {
		return nil, "", fmt.Errorf("%w: from address %q is invalid", ErrCompose, in.From.Address)
	}
	// Injection guard: every value that lands inside a header line or a
	// header parameter is CR/LF-free before anything is written (addresses
	// arrived pre-parsed, so only display names, the subject, and the
	// attachment metadata need the check).
	names := []string{in.From.Name, in.Subject}
	for _, a := range in.To {
		names = append(names, a.Name)
	}
	for _, a := range in.Cc {
		names = append(names, a.Name)
	}
	for _, a := range in.Bcc {
		names = append(names, a.Name)
	}
	for _, at := range in.Attachments {
		names = append(names, at.Filename, at.ContentType)
	}
	for _, n := range names {
		if strings.ContainsAny(n, "\r\n") {
			return nil, "", fmt.Errorf("%w: CR or LF in a header value", ErrCompose)
		}
	}
	for _, threading := range []string{in.InReplyTo, in.References} {
		if threading != "" && !messageIDRe.MatchString(threading) {
			return nil, "", fmt.Errorf("%w: threading header %q is not a message-id", ErrCompose, threading)
		}
	}

	messageID := fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), randomHex8(), domain)
	var b bytes.Buffer
	h := func(name, value string) {
		b.WriteString(name + ": " + value + "\r\n")
	}
	h("From", in.From.String())
	if len(in.To) > 0 {
		h("To", addrList(in.To))
	}
	if len(in.Cc) > 0 {
		h("Cc", addrList(in.Cc))
	}
	h("Subject", encodeSubject(in.Subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", messageID)
	h("MIME-Version", "1.0")
	if in.InReplyTo != "" {
		h("In-Reply-To", in.InReplyTo)
	}
	if in.References != "" {
		h("References", in.References)
	}

	body := bodySection(in)
	if len(in.Attachments) == 0 {
		b.Write(body)
	} else {
		boundary := "ncgo-mixed-" + randomHex8()
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
		writeMIMEPart(&b, boundary, body)
		for _, at := range in.Attachments {
			writeMIMEPart(&b, boundary, attachmentLeaf(at))
		}
		fmt.Fprintf(&b, "--%s--\r\n", boundary)
	}
	if b.Len() > maxComposedMessageSize {
		return nil, "", ErrMessageTooLarge
	}
	return b.Bytes(), messageID, nil
}

// bodySection renders the body part WITHOUT the outer blank line when it is
// a single text leaf, and as a complete multipart/alternative section
// otherwise: in both cases it is a self-contained MIME entity (headers,
// blank line, body) ready to be the whole message body or one mixed part.
func bodySection(in ComposeInput) []byte {
	plain := textLeaf("text/plain", in.BodyPlain)
	if in.BodyHTML == "" {
		return plain
	}
	boundary := "ncgo-alt-" + randomHex8()
	var b bytes.Buffer
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	writeMIMEPart(&b, boundary, plain)
	writeMIMEPart(&b, boundary, textLeaf("text/html", in.BodyHTML))
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// textLeaf renders one text/* entity: charset=utf-8, quoted-printable body
// (the stdlib QP writer wraps at 76 columns).
func textLeaf(contentType, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\r\n", contentType)
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	w := quotedprintable.NewWriter(&b)
	_, _ = io.Copy(w, strings.NewReader(body)) // into a bytes.Buffer: cannot fail
	_ = w.Close()                              // same: Close only flushes into it
	return b.Bytes()
}

// attachmentLeaf renders one attachment entity: Content-Type (with an RFC
// 2231 name parameter when the filename is non-ASCII), base64 content at 76
// columns, and an attachment Content-Disposition carrying the filename.
func attachmentLeaf(at OutgoingAttachment) []byte {
	ct := at.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	var b bytes.Buffer
	typeParams := map[string]string{}
	if at.Filename != "" {
		typeParams["name"] = at.Filename
	}
	fmt.Fprintf(&b, "Content-Type: %s\r\n", mime.FormatMediaType(ct, typeParams))
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	dispParams := map[string]string{}
	if at.Filename != "" {
		dispParams["filename"] = at.Filename
	}
	fmt.Fprintf(&b, "Content-Disposition: %s\r\n\r\n", mime.FormatMediaType("attachment", dispParams))
	writeBase64(&b, at.Data)
	return b.Bytes()
}

// writeMIMEPart appends one entity to a multipart body: boundary delimiter,
// the entity (headers + blank line + body), and a terminating CRLF.
func writeMIMEPart(b *bytes.Buffer, boundary string, entity []byte) {
	fmt.Fprintf(b, "--%s\r\n", boundary)
	b.Write(entity)
	b.WriteString("\r\n")
}

// writeBase64 encodes data as 76-column base64 lines (57 source bytes make
// exactly 76 output characters).
func writeBase64(b *bytes.Buffer, data []byte) {
	const chunk = 57
	for len(data) > chunk {
		b.WriteString(base64.StdEncoding.EncodeToString(data[:chunk]))
		b.WriteString("\r\n")
		data = data[chunk:]
	}
	if len(data) > 0 {
		b.WriteString(base64.StdEncoding.EncodeToString(data))
	}
}

// addrList renders a parsed address list for a header line;
// mail.Address.String already quotes and RFC 2047-encodes display names.
func addrList(addrs []mail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
}

// encodeSubject Q-encodes a non-ASCII subject (RFC 2047); ASCII passes
// through verbatim.
func encodeSubject(subject string) string {
	for i := 0; i < len(subject); i++ {
		if subject[i] >= 0x80 {
			return mime.QEncoding.Encode("utf-8", subject)
		}
	}
	return subject
}

// addressDomain returns the part after the final '@' — the Message-ID's
// right-hand side.
func addressDomain(addr string) (string, bool) {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || at == len(addr)-1 {
		return "", false
	}
	return addr[at+1:], true
}

// randomHex8 returns 8 random bytes hex-encoded (16 chars): the entropy in
// Message-IDs and MIME boundaries.
func randomHex8() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failure is unrecoverable for every other caller in the
		// repo as well; fall back to the timestamp so composing still works.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
