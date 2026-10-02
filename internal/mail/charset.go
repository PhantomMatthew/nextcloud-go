package mail

import (
	"io"
	"strings"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

// charset.go decodes message bodies out of their declared charset into
// UTF-8 (ADR-0108 §6). golang.org/x/text — an indirect dependency since
// before the Mail epic — is promoted to direct for exactly this; it is the
// epic's one sanctioned dependency exception.

// DecodeBody wraps r with the decoder for charsetLabel (a MIME charset
// parameter as it arrives in Content-Type). An empty, unknown, or
// unsupported label passes the body through unchanged (treated as UTF-8):
// charset labels are server-controlled junk half the time, and a body that
// cannot be decoded must still render, never error. htmlindex resolves the
// WHATWG label set (case-insensitive, with aliases), so "gbk", "GB2312",
// "Shift_JIS", "latin1", … all land on their encoding; labels the standard
// deliberately excludes (UTF-7) fall through to passthrough.
func DecodeBody(r io.Reader, charsetLabel string) io.Reader {
	label := strings.TrimSpace(charsetLabel)
	if label == "" {
		return r
	}
	enc, err := htmlindex.Get(label)
	if err != nil {
		return r
	}
	return transform.NewReader(r, enc.NewDecoder())
}
