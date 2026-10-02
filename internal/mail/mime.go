package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
)

// mime.go is the M4 whole-message walker (ADR-0108 §6): stdlib net/mail +
// mime + mime/multipart only. ParseMessage renders the first text/plain and
// first text/html leaf (decoded through DecodeBody with the part's declared
// charset) and enumerates the attachments; ExtractAttachment re-walks the
// same bytes to fetch one attachment's decoded content. Malformed trees
// return whatever parsed so far — a broken message must never lose the
// whole body, and never panic.
//
// Attachment INDEX STABILITY: indices count attachment leaves only, dense
// from 0, assigned in depth-first leaf order of one raw message. The detail
// endpoint and the attachment download walk the same raw bytes with the same
// rules, so an index from GET .../messages/{mid} is always valid for
// GET .../messages/{mid}/attachments/{index} of the same message.

// maxRawMessageSize bounds one message body fetch/parse (the IMAP reader
// independently caps a wire literal at the same size).
const maxRawMessageSize = 32 << 20

var (
	// ErrMessageTooLarge reports a message over maxRawMessageSize → 413.
	ErrMessageTooLarge = errors.New("mail: message exceeds the 32 MiB limit")
	// ErrNoAttachment reports an attachment index the message does not have
	// → 404.
	ErrNoAttachment = errors.New("mail: no attachment with that index")
)

// Attachment is one attachment leaf's metadata. Size is the DECODED byte
// count (after Content-Transfer-Encoding).
type Attachment struct {
	Index       int
	Filename    string
	ContentType string
	Size        int64
}

// ParsedBody is the render model of one message: the first plain and first
// HTML body leaf (UTF-8) plus every attachment's metadata.
type ParsedBody struct {
	TextPlain   string
	TextHTML    string
	Attachments []Attachment
}

// ParseMessage walks raw (one whole RFC 822 message) into its render model.
// The only hard failure is the size guard; anything malformed degrades to
// the best partial parse.
func ParseMessage(raw []byte) (*ParsedBody, error) {
	if len(raw) > maxRawMessageSize {
		return nil, ErrMessageTooLarge
	}
	w := newWalker(-1)
	w.walkMessage(raw)
	return &w.parsed, nil
}

// ExtractAttachment re-walks raw and returns the metadata and DECODED bytes
// of the attachment with index (the ParseMessage walk order — see the file
// comment). An index the message does not have is ErrNoAttachment.
func ExtractAttachment(raw []byte, index int) (Attachment, []byte, error) {
	if len(raw) > maxRawMessageSize {
		return Attachment{}, nil, ErrMessageTooLarge
	}
	if index < 0 {
		return Attachment{}, nil, ErrNoAttachment
	}
	w := newWalker(index)
	w.walkMessage(raw)
	if w.found == nil {
		return Attachment{}, nil, ErrNoAttachment
	}
	return *w.found, w.foundBytes, nil
}

// walker carries one depth-first traversal. want >= 0 selects
// ExtractAttachment mode: only the wanted attachment's bytes are kept and
// the render model is not populated.
type walker struct {
	want       int
	nextAtt    int
	havePlain  bool
	haveHTML   bool
	parsed     ParsedBody
	found      *Attachment
	foundBytes []byte
}

func newWalker(want int) *walker { return &walker{want: want} }

func (w *walker) walkMessage(raw []byte) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		// Even a message whose headers do not parse must not be lost: serve
		// the raw bytes as an undifferentiated text/plain body.
		w.parsed.TextPlain = string(raw)
		w.havePlain = true
		return
	}
	w.walkPart(textproto.MIMEHeader(msg.Header), msg.Body)
}

// walkPart descends multipart trees recursively; everything else is a leaf.
func (w *walker) walkPart(h textproto.MIMEHeader, body io.Reader) {
	mt, params := partMediaType(h.Get("Content-Type"))
	if mt == "" {
		mt = "text/plain" // RFC 2045 default
	}
	if strings.HasPrefix(mt, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return // a multipart without a boundary cannot be walked
		}
		mr := multipart.NewReader(body, boundary)
		for {
			p, err := mr.NextPart()
			if err != nil {
				return // io.EOF or a truncated tree: keep what parsed so far
			}
			w.walkPart(p.Header, p)
		}
	}
	w.leaf(h, mt, params, body)
}

// leaf classifies one non-multipart part: the first plain/html leaf without
// an attachment disposition is a body; a Content-Disposition attachment,
// any non-text leaf, and message/rfc822 are attachments. Further text
// leaves are neither (v1 renders one body per type).
func (w *walker) leaf(h textproto.MIMEHeader, mt string, params map[string]string, body io.Reader) {
	disp, dispParams := partMediaType(h.Get("Content-Disposition"))
	if disp != "attachment" {
		if mt == "text/plain" && !w.havePlain {
			w.havePlain = true
			if w.want < 0 {
				w.parsed.TextPlain = w.readText(h, params, body)
			}
			return
		}
		if mt == "text/html" && !w.haveHTML {
			w.haveHTML = true
			if w.want < 0 {
				w.parsed.TextHTML = w.readText(h, params, body)
			}
			return
		}
		if strings.HasPrefix(mt, "text/") {
			return // a second leaf of an already-taken text type
		}
	}
	att := Attachment{
		Index:       w.nextAtt,
		Filename:    partFilename(dispParams, params),
		ContentType: mt,
	}
	w.nextAtt++
	switch {
	case w.want < 0:
		att.Size = w.countDecoded(h, body)
		w.parsed.Attachments = append(w.parsed.Attachments, att)
	case att.Index == w.want:
		data := w.readDecoded(h, body)
		att.Size = int64(len(data))
		found := att
		w.found = &found
		w.foundBytes = data
	}
}

// readText decodes a body leaf: transfer encoding first, then the declared
// charset into UTF-8 (unknown charsets pass through, DecodeBody never
// fails).
func (w *walker) readText(h textproto.MIMEHeader, params map[string]string, body io.Reader) string {
	data := w.readDecoded(h, body)
	decoded, err := io.ReadAll(DecodeBody(bytes.NewReader(data), params["charset"]))
	if err != nil {
		return string(data) // a failing charset reader keeps the raw decode
	}
	return string(decoded)
}

// readDecoded drains a leaf through its transfer-encoding decoder. A
// corrupt encoding still yields the partial decode; rendering is
// best-effort, so the partial bytes are used as-is (io.Copy's error is
// intentionally unreportable here — lint-excluded).
func (w *walker) readDecoded(h textproto.MIMEHeader, body io.Reader) []byte {
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, cteReader(h, body))
	return buf.Bytes()
}

// countDecoded measures a leaf's decoded size without buffering it.
func (w *walker) countDecoded(h textproto.MIMEHeader, body io.Reader) int64 {
	n, _ := io.Copy(io.Discard, cteReader(h, body))
	return n
}

// cteReader wraps a leaf with its Content-Transfer-Encoding decoder. Inside
// a multipart tree the stdlib reader has already transparently decoded
// quoted-printable (and hidden the header), so only base64 ever needs work
// there; the top-level body still carries QP. Unknown encodings pass
// through byte-exact.
func cteReader(h textproto.MIMEHeader, body io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding"))) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	default:
		return body
	}
}

// previewRunes caps the M6 list preview (runes, not bytes — a CJK
// character costs one).
const previewRunes = 200

// previewText renders the list-view preview of a plain-text body:
// whitespace runs collapse to single spaces (strings.Fields already trims),
// capped at previewRunes runes. A message without a plain part previews
// empty — HTML is deliberately NOT stripped (the M6 preview is plain-only;
// sanitization is M7 scope).
func previewText(plain string) string {
	joined := strings.Join(strings.Fields(plain), " ")
	runes := []rune(joined)
	if len(runes) > previewRunes {
		return string(runes[:previewRunes])
	}
	return joined
}

// partMediaType parses a Content-Type/Content-Disposition header value. A
// malformed parameter list keeps the lower-cased base token: a broken
// header must not hide the declared type.
func partMediaType(raw string) (string, map[string]string) {
	if raw == "" {
		return "", nil
	}
	mt, params, err := mime.ParseMediaType(raw)
	if err != nil {
		base, _, _ := strings.Cut(raw, ";")
		return strings.ToLower(strings.TrimSpace(base)), nil
	}
	return mt, params
}

// partFilename resolves an attachment's display name: the
// Content-Disposition filename first, then the Content-Type name parameter.
// mime.ParseMediaType has already assembled RFC 2231 continuations and
// charset parameters; RFC 2047 encoded-words decode on top (with charsets
// routed through DecodeBody, so GBK words work); a decode failure keeps the
// raw form.
func partFilename(dispParams, typeParams map[string]string) string {
	name := dispParams["filename"]
	if name == "" {
		name = typeParams["name"]
	}
	if name == "" {
		return ""
	}
	dec := mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		return DecodeBody(input, charset), nil
	}}
	out, err := dec.DecodeHeader(name)
	if err != nil {
		return name
	}
	return out
}
