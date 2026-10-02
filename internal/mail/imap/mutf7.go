package imap

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
)

// mutf7B64 is the RFC 2152 modified base64 alphabet: ',' substitutes for '/'
// and padding is omitted.
var mutf7B64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// DecodeMailboxName decodes an IMAP modified UTF-7 (RFC 2152) mailbox name
// into UTF-8 for display: printable ASCII passes through, "&-" is a literal
// '&', and "&<modified-base64>-" decodes as UTF-16BE units. A malformed
// sequence returns the input unchanged — one bad server name must never
// break the mailbox list, and the wire form is still usable for EXAMINE.
func DecodeMailboxName(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		rel := strings.IndexByte(s[i:], '-')
		if rel < 0 {
			return s // unterminated '&' sequence
		}
		seg := s[i+1 : i+rel]
		if seg == "" { // "&-" is a literal '&'
			b.WriteByte('&')
			i += 2
			continue
		}
		raw, err := mutf7B64.DecodeString(seg)
		if err != nil || len(raw)%2 != 0 {
			return s
		}
		u16 := make([]uint16, len(raw)/2)
		for j := range u16 {
			u16[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}
		b.WriteString(string(utf16.Decode(u16)))
		i += rel + 1
	}
	return b.String()
}
