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
	"regexp"
	"strings"
	"testing"
)

// compose_test.go asserts the M5 composer's output STRUCTURALLY: every
// message is parsed back with net/mail and walked part by part, so the
// checks pin the MIME tree shape, the transfer encodings, and the header
// contract (Bcc never in the bytes, threading passthrough, injection guard,
// 25 MiB cap) rather than exact byte sequences (boundaries and Message-IDs
// carry randomness).

// parseComposed parses the composed message and returns it.
func parseComposed(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("composed message does not parse: %v", err)
	}
	return msg
}

// mimePart is one walked multipart part: headers as parsed (note:
// mime/multipart transparently decodes quoted-printable parts and hides
// their CTE header — text part bodies arrive already decoded) plus the part
// body.
type mimePart struct {
	header textproto.MIMEHeader
	body   string
}

// parts walks one multipart entity into its parts.
func parts(t *testing.T, msg *mail.Message) []mimePart {
	t.Helper()
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		t.Fatalf("Content-Type = %q, want multipart (err=%v)", msg.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var out []mimePart
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("multipart walk: %v", err)
		}
		body, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		out = append(out, mimePart{header: p.Header, body: string(body)})
	}
}

// qpDecode decodes a quoted-printable body (the composed top-level text
// leaf — inside multiparts the reader has already decoded it).
func qpDecode(t *testing.T, s string) string {
	t.Helper()
	body, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(s)))
	if err != nil {
		t.Fatalf("quoted-printable decode: %v", err)
	}
	return string(body)
}

// crlf normalizes LF to the CRLF canonical form quoted-printable encoding
// produces on the wire.
func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func baseCompose() ComposeInput {
	return ComposeInput{
		From:      mail.Address{Name: "Alice A", Address: "alice@example.com"},
		To:        []mail.Address{{Name: "Bob", Address: "bob@example.com"}},
		Subject:   "Hello there",
		BodyPlain: "line one\nline two",
	}
}

func TestComposePlainOnly(t *testing.T) {
	t.Parallel()
	in := baseCompose()
	in.Bcc = []mail.Address{{Address: "hidden@example.com"}}
	raw, err := Compose(in)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	msg := parseComposed(t, raw)
	froms, err := mail.ParseAddressList(msg.Header.Get("From"))
	if err != nil || len(froms) != 1 || froms[0].Address != "alice@example.com" || froms[0].Name != "Alice A" {
		t.Errorf("From = %q (parsed %+v, err=%v)", msg.Header.Get("From"), froms, err)
	}
	tos, err := mail.ParseAddressList(msg.Header.Get("To"))
	if err != nil || len(tos) != 1 || tos[0].Address != "bob@example.com" || tos[0].Name != "Bob" {
		t.Errorf("To = %q (parsed %+v, err=%v)", msg.Header.Get("To"), tos, err)
	}
	if msg.Header.Get("Subject") != "Hello there" {
		t.Errorf("Subject = %q", msg.Header.Get("Subject"))
	}
	if msg.Header.Get("MIME-Version") != "1.0" {
		t.Errorf("MIME-Version = %q", msg.Header.Get("MIME-Version"))
	}
	if _, err := mail.ParseDate(msg.Header.Get("Date")); err != nil {
		t.Errorf("Date %q does not parse: %v", msg.Header.Get("Date"), err)
	}
	if id := msg.Header.Get("Message-ID"); !regexp.MustCompile(`^<\d+\.[0-9a-f]{16}@example\.com>$`).MatchString(id) {
		t.Errorf("Message-ID = %q, want <unixnano.hex16@example.com>", id)
	}
	// Bcc is an envelope-only concept: neither the header nor the address
	// may appear anywhere in the message bytes.
	if msg.Header.Get("Bcc") != "" || bytes.Contains(raw, []byte("hidden@example.com")) {
		t.Error("Bcc leaked into the composed bytes")
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "text/plain" || params["charset"] != "utf-8" {
		t.Errorf("Content-Type = %q params = %v (err=%v)", mt, params, err)
	}
	if cte := msg.Header.Get("Content-Transfer-Encoding"); cte != "quoted-printable" {
		t.Errorf("CTE = %q, want quoted-printable", cte)
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := qpDecode(t, string(body)); got != crlf(in.BodyPlain) {
		t.Errorf("plain body = %q, want %q", got, crlf(in.BodyPlain))
	}
}

func TestComposeAlternative(t *testing.T) {
	t.Parallel()
	in := baseCompose()
	in.BodyHTML = "<p>line one</p>"
	raw, err := Compose(in)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	msg := parseComposed(t, raw)
	if mt, _, _ := mime.ParseMediaType(msg.Header.Get("Content-Type")); mt != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mt)
	}
	ps := parts(t, msg)
	if len(ps) != 2 {
		t.Fatalf("part count = %d, want 2", len(ps))
	}
	// Part order: plain first, HTML second. The multipart reader has
	// already QP-decoded both bodies (and hidden their CTE header).
	for i, want := range []struct {
		mt   string
		body string
	}{
		{"text/plain", in.BodyPlain},
		{"text/html", in.BodyHTML},
	} {
		if mt, _, _ := mime.ParseMediaType(ps[i].header.Get("Content-Type")); mt != want.mt {
			t.Errorf("part %d Content-Type = %q, want %q", i, mt, want.mt)
		}
		if ps[i].body != crlf(want.body) {
			t.Errorf("part %d body = %q, want %q", i, ps[i].body, crlf(want.body))
		}
	}
}

func TestComposeMixedAttachmentsRoundTrip(t *testing.T) {
	t.Parallel()
	in := baseCompose()
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01, 0x02, 0x03}
	txt := bytes.Repeat([]byte("attach-me\n"), 200) // multi-line base64 wraps
	in.Attachments = []OutgoingAttachment{
		{Filename: "pix.png", ContentType: "image/png", Data: png},
		{Filename: "数据.txt", ContentType: "text/plain", Data: txt},
	}
	raw, err := Compose(in)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	msg := parseComposed(t, raw)
	if mt, _, _ := mime.ParseMediaType(msg.Header.Get("Content-Type")); mt != "multipart/mixed" {
		t.Fatalf("Content-Type = %q, want multipart/mixed", mt)
	}
	ps := parts(t, msg)
	if len(ps) != 3 {
		t.Fatalf("part count = %d, want 3 (body + 2 attachments)", len(ps))
	}
	// Part 0 is the body entity (a single text leaf here — the reader has
	// already QP-decoded it).
	if mt, _, _ := mime.ParseMediaType(ps[0].header.Get("Content-Type")); mt != "text/plain" {
		t.Errorf("body part Content-Type = %q, want text/plain", mt)
	}
	if ps[0].body != crlf(in.BodyPlain) {
		t.Errorf("body part = %q, want %q", ps[0].body, crlf(in.BodyPlain))
	}
	for i, want := range in.Attachments {
		h := ps[i+1].header
		mt, typeParams, err := mime.ParseMediaType(h.Get("Content-Type"))
		if err != nil || mt != want.ContentType {
			t.Errorf("attachment %d Content-Type = %q (err=%v)", i, mt, err)
		}
		if typeParams["name"] != want.Filename {
			t.Errorf("attachment %d name = %q, want %q", i, typeParams["name"], want.Filename)
		}
		disp, dispParams, err := mime.ParseMediaType(h.Get("Content-Disposition"))
		if err != nil || disp != "attachment" {
			t.Errorf("attachment %d disposition = %q (err=%v)", i, disp, err)
		}
		if dispParams["filename"] != want.Filename {
			t.Errorf("attachment %d filename = %q, want %q (RFC 2231 round-trip)", i, dispParams["filename"], want.Filename)
		}
		if cte := h.Get("Content-Transfer-Encoding"); cte != "base64" {
			t.Errorf("attachment %d CTE = %q, want base64", i, cte)
		}
		compact := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, ps[i+1].body)
		decoded, err := base64.StdEncoding.DecodeString(compact)
		if err != nil {
			t.Fatalf("attachment %d base64 does not decode: %v", i, err)
		}
		if !bytes.Equal(decoded, want.Data) {
			t.Errorf("attachment %d decoded %d bytes, want the %d source bytes", i, len(decoded), len(want.Data))
		}
	}
	// The wrapped base64 lines stay at 76 columns.
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 76 && !strings.HasPrefix(line, "Content-") {
			t.Errorf("line of %d cols exceeds 76: %.40q…", len(line), line)
		}
	}
}

func TestComposeEncodedSubject(t *testing.T) {
	t.Parallel()
	in := baseCompose()
	in.Subject = "Rechnung für März — 发票"
	raw, err := Compose(in)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	msg := parseComposed(t, raw)
	headerSubject := msg.Header.Get("Subject")
	if strings.Contains(headerSubject, "发票") {
		t.Error("non-ASCII subject was not encoded")
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(headerSubject)
	if err != nil {
		t.Fatalf("subject does not decode: %v", err)
	}
	if decoded != in.Subject {
		t.Errorf("decoded subject = %q, want %q", decoded, in.Subject)
	}
}

func TestComposeThreadingHeaders(t *testing.T) {
	t.Parallel()
	in := baseCompose()
	in.InReplyTo = "<abc@def.example>"
	in.References = "<root@def.example>"
	raw, err := Compose(in)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	msg := parseComposed(t, raw)
	if msg.Header.Get("In-Reply-To") != in.InReplyTo {
		t.Errorf("In-Reply-To = %q", msg.Header.Get("In-Reply-To"))
	}
	if msg.Header.Get("References") != in.References {
		t.Errorf("References = %q", msg.Header.Get("References"))
	}
}

func TestComposeInjectionMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(in *ComposeInput)
	}{
		{"subject CRLF", func(in *ComposeInput) { in.Subject = "hi\r\nBcc: evil@example.com" }},
		{"from name CRLF", func(in *ComposeInput) { in.From.Name = "Alice\nBcc: evil@example.com" }},
		{"to name CRLF", func(in *ComposeInput) { in.To[0].Name = "Bob\rEvil" }},
		{"cc name CRLF", func(in *ComposeInput) {
			in.Cc = []mail.Address{{Name: "x\n", Address: "c@example.com"}}
		}},
		{"bcc name CRLF", func(in *ComposeInput) {
			in.Bcc = []mail.Address{{Name: "x\r", Address: "b@example.com"}}
		}},
		{"filename CRLF", func(in *ComposeInput) {
			in.Attachments = []OutgoingAttachment{{Filename: "a\nb.txt", Data: []byte("x")}}
		}},
		{"content type CRLF", func(in *ComposeInput) {
			in.Attachments = []OutgoingAttachment{{ContentType: "text/plain\r\nX-Evil: 1", Data: []byte("x")}}
		}},
		{"in-reply-to no brackets", func(in *ComposeInput) { in.InReplyTo = "abc@def" }},
		{"in-reply-to CRLF", func(in *ComposeInput) { in.InReplyTo = "<a>\r\nX: 1" }},
		{"references two ids", func(in *ComposeInput) { in.References = "<a> <b>" }},
		{"no body", func(in *ComposeInput) { in.BodyPlain = "" }},
		{"from missing host", func(in *ComposeInput) { in.From.Address = "alice@" }},
		{"from missing at", func(in *ComposeInput) { in.From.Address = "alice" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := baseCompose()
			tc.mutate(&in)
			if _, err := Compose(in); !errors.Is(err, ErrCompose) {
				t.Errorf("Compose: err = %v, want ErrCompose", err)
			}
		})
	}
}

func TestComposeSizeCap(t *testing.T) {
	t.Parallel()
	in := baseCompose()
	// 20 MiB of source bytes base64-expand to ~26.7 MiB — over the 25 MiB
	// composed cap.
	in.Attachments = []OutgoingAttachment{{Filename: "big.bin", Data: make([]byte, 20<<20)}}
	if _, err := Compose(in); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Compose: err = %v, want ErrMessageTooLarge", err)
	}
}
