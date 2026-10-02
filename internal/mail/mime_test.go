package mail

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// mime_test.go covers the M4 whole-message walker: body extraction across
// the multipart shapes, attachment enumeration/extraction, filename
// decoding (RFC 2231 + RFC 2047), charset bodies, and the malformed-input
// matrix (best-effort, never a panic).

func TestParseMessageMixedWithAttachment(t *testing.T) {
	t.Parallel()
	raw := "From: a@example.com\r\n" +
		"Content-Type: multipart/mixed; boundary=mix\r\n" +
		"\r\n" +
		"--mix\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"hello body\r\n" +
		"--mix\r\n" +
		"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"UERGJXZlcnNpb24=\r\n" + // "PDF%version"
		"--mix--\r\n"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pb.TextPlain, "hello body") {
		t.Errorf("plain = %q", pb.TextPlain)
	}
	if pb.TextHTML != "" {
		t.Errorf("html = %q, want empty", pb.TextHTML)
	}
	want := []Attachment{{Index: 0, Filename: "report.pdf", ContentType: "application/pdf", Size: 11}}
	if !reflect.DeepEqual(pb.Attachments, want) {
		t.Errorf("attachments = %+v, want %+v", pb.Attachments, want)
	}
	att, data, err := ExtractAttachment([]byte(raw), 0)
	if err != nil {
		t.Fatal(err)
	}
	if att != want[0] || string(data) != "PDF%version" {
		t.Errorf("extract = %+v %q", att, data)
	}
}

func TestParseMessageAlternative(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: multipart/alternative; boundary=alt\r\n" +
		"\r\n" +
		"--alt\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"plain version\r\n" +
		"--alt\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<p>html version</p>\r\n" +
		"--alt--\r\n"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pb.TextPlain, "plain version") || !strings.Contains(pb.TextHTML, "html version") {
		t.Errorf("bodies = %q / %q", pb.TextPlain, pb.TextHTML)
	}
	if len(pb.Attachments) != 0 {
		t.Errorf("attachments = %+v, want none", pb.Attachments)
	}
}

// TestParseMessageNested walks mixed → alternative → leaves; the depth-first
// attachment order is the wire order across nesting levels.
func TestParseMessageNested(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: multipart/mixed; boundary=out\r\n" +
		"\r\n" +
		"--out\r\n" +
		"Content-Type: multipart/alternative; boundary=in\r\n" +
		"\r\n" +
		"--in\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"nested plain\r\n" +
		"--in\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<b>nested html</b>\r\n" +
		"--in--\r\n" +
		"--out\r\n" +
		"Content-Type: image/png; name=\"a.png\"\r\n" +
		"\r\n" +
		"PNGBYTES\r\n" +
		"--out\r\n" +
		"Content-Type: application/zip\r\n" +
		"Content-Disposition: attachment; filename=\"z.zip\"\r\n" +
		"\r\n" +
		"ZIPBYTES\r\n" +
		"--out--\r\n"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pb.TextPlain, "nested plain") || !strings.Contains(pb.TextHTML, "nested html") {
		t.Errorf("bodies = %q / %q", pb.TextPlain, pb.TextHTML)
	}
	wantTypes := []string{"image/png", "application/zip"}
	if len(pb.Attachments) != 2 {
		t.Fatalf("attachments = %+v", pb.Attachments)
	}
	for i, at := range pb.Attachments {
		if at.Index != i || at.ContentType != wantTypes[i] {
			t.Errorf("attachment %d = %+v", i, at)
		}
	}
	// An inline (no disposition) non-text leaf is an attachment too; the
	// first attachment's bytes extract by the shared index.
	att, data, err := ExtractAttachment([]byte(raw), 0)
	if err != nil {
		t.Fatal(err)
	}
	if att.Filename != "a.png" || !strings.Contains(string(data), "PNGBYTES") {
		t.Errorf("extract 0 = %+v %q", att, data)
	}
	if _, _, err := ExtractAttachment([]byte(raw), 2); !errors.Is(err, ErrNoAttachment) {
		t.Errorf("extract 2: err = %v, want ErrNoAttachment", err)
	}
	if _, _, err := ExtractAttachment([]byte(raw), -1); !errors.Is(err, ErrNoAttachment) {
		t.Errorf("extract -1: err = %v, want ErrNoAttachment", err)
	}
}

// TestParseMessageRFC2231Filename: continuation segments and an extended
// charset parameter both assemble through mime.ParseMediaType.
func TestParseMessageRFC2231Filename(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"x\r\n" +
		"--b\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename*0=\"long_na\"; filename*1=\"me.pdf\"\r\n" +
		"\r\n" +
		"AA\r\n" +
		"--b\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename*=utf-8''%E4%B8%AD%E6%96%87.pdf\r\n" +
		"\r\n" +
		"BB\r\n" +
		"--b--\r\n"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(pb.Attachments) != 2 {
		t.Fatalf("attachments = %+v", pb.Attachments)
	}
	if pb.Attachments[0].Filename != "long_name.pdf" {
		t.Errorf("continuation filename = %q", pb.Attachments[0].Filename)
	}
	if pb.Attachments[1].Filename != "中文.pdf" {
		t.Errorf("extended filename = %q", pb.Attachments[1].Filename)
	}
}

// TestParseMessageEncodedWordFilename: an RFC 2047 encoded-word filename
// decodes, GBK words included (DecodeBody backs the WordDecoder).
func TestParseMessageEncodedWordFilename(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"x\r\n" +
		"--b\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"=?UTF-8?B?5Lit5paHLnBkZg==?=\"\r\n" +
		"\r\n" +
		"AA\r\n" +
		"--b\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"=?GBK?B?xMeter?=\"\r\n" +
		"\r\n" +
		"BB\r\n" +
		"--b--\r\n"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if pb.Attachments[0].Filename != "中文.pdf" {
		t.Errorf("utf-8 word filename = %q", pb.Attachments[0].Filename)
	}
	// 0xD6D0 is 中 in GBK.
	raw2 := strings.Replace(raw, "filename=\"=?GBK?B?xMeter?=\"", "filename=\"=?GBK?B?1tAucGRm?=\"", 1)
	pb2, err := ParseMessage([]byte(raw2))
	if err != nil {
		t.Fatal(err)
	}
	if pb2.Attachments[1].Filename != "中.pdf" {
		t.Errorf("gbk word filename = %q", pb2.Attachments[1].Filename)
	}
}

// TestParseMessageGBKHTML: an HTML body in GBK decodes to UTF-8.
func TestParseMessageGBKHTML(t *testing.T) {
	t.Parallel()
	gbk := []byte("<p>")
	gbk = append(gbk, 0xD6, 0xD0, 0xCE, 0xC4) // 中文 in GBK
	gbk = append(gbk, []byte("</p>")...)
	var buf bytes.Buffer
	buf.WriteString("Content-Type: text/html; charset=gbk\r\n\r\n")
	buf.Write(gbk)
	pb, err := ParseMessage(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if pb.TextHTML != "<p>中文</p>" {
		t.Errorf("html = %q", pb.TextHTML)
	}
}

// TestParseMessageRFC822Attachment: message/rfc822 is an attachment, never
// recursed into.
func TestParseMessageRFC822Attachment(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"see attached\r\n" +
		"--b\r\n" +
		"Content-Type: message/rfc822; name=\"fwd.eml\"\r\n" +
		"\r\n" +
		"Subject: inner\r\n\r\ninner body\r\n" + //nolint:dupword // test fixture names the inner message twice
		"--b--\r\n"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if pb.TextPlain != "see attached" {
		t.Errorf("plain = %q (the inner message must not leak into the body)", pb.TextPlain)
	}
	if len(pb.Attachments) != 1 || pb.Attachments[0].ContentType != "message/rfc822" ||
		pb.Attachments[0].Filename != "fwd.eml" {
		t.Errorf("attachments = %+v", pb.Attachments)
	}
	_, data, err := ExtractAttachment([]byte(raw), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "inner body") {
		t.Errorf("rfc822 bytes = %q", data)
	}
}

// TestParseMessageTruncated: a multipart that never closes still returns
// what parsed so far.
func TestParseMessageTruncated(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"partial body, stream ends here"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pb.TextPlain, "partial body") {
		t.Errorf("plain = %q", pb.TextPlain)
	}

	// Garbage headers degrade to a raw text/plain dump — never an error,
	// never a panic, never an empty message.
	garbage := ParseMessageMust(t, []byte("\x00\x01\x02 binary junk \xff"))
	if garbage.TextPlain != "\x00\x01\x02 binary junk \xff" {
		t.Errorf("garbage plain = %q", garbage.TextPlain)
	}
}

func ParseMessageMust(t *testing.T, raw []byte) *ParsedBody {
	t.Helper()
	pb, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	return pb
}

// TestParseMessageNoTextPart: an attachment-only message has empty bodies.
func TestParseMessageNoTextPart(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"data.bin\"\r\n" +
		"\r\n" +
		"\x00\x01\x02"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if pb.TextPlain != "" || pb.TextHTML != "" {
		t.Errorf("bodies = %q / %q, want empty", pb.TextPlain, pb.TextHTML)
	}
	if len(pb.Attachments) != 1 || pb.Attachments[0].Size != 3 {
		t.Errorf("attachments = %+v", pb.Attachments)
	}
}

// TestParseMessageQuotedPrintable: QP decodes at the top level and inside
// multipart (where the stdlib reader decodes transparently).
func TestParseMessageQuotedPrintable(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"caf=C3=A9 and a soft=\r\n break\r\n"
	pb, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if pb.TextPlain != "café and a soft break\r\n" {
		t.Errorf("plain = %q", pb.TextPlain)
	}

	inMulti := "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n" +
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=C3=A9\r\n--b--\r\n"
	pb2, err := ParseMessage([]byte(inMulti))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pb2.TextPlain, "café") {
		t.Errorf("multipart plain = %q", pb2.TextPlain)
	}
}

// TestParseMessageTooLarge: the 32 MiB guard is a typed error on both entry
// points.
func TestParseMessageTooLarge(t *testing.T) {
	t.Parallel()
	raw := make([]byte, maxRawMessageSize+1)
	if _, err := ParseMessage(raw); !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("ParseMessage: err = %v", err)
	}
	if _, _, err := ExtractAttachment(raw, 0); !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("ExtractAttachment: err = %v", err)
	}
}

// TestParseMessageMalformedMatrix hammers broken trees: everything returns,
// nothing panics.
func TestParseMessageMalformedMatrix(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"\r\n",
		"Content-Type: multipart/mixed\r\n\r\nno boundary anywhere",
		"Content-Type: multipart/mixed; boundary=\"\"\r\n\r\n--\r\n",
		"Content-Type: text/plain; charset=\"\r\n\r\nbroken params",
		"Content-Disposition: attachment; filename\r\n\r\nx",
		"Content-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!not-base64!!!",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/html\r\n\r\n<b>unclosed",
	}
	for _, raw := range cases {
		pb, err := ParseMessage([]byte(raw))
		if err != nil {
			t.Errorf("ParseMessage(%q): %v", raw, err)
			continue
		}
		if pb == nil {
			t.Errorf("ParseMessage(%q): nil body", raw)
		}
	}
}
