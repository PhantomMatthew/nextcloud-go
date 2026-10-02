package mail

import (
	"bytes"
	"io"
	"testing"
)

// TestDecodeBody runs the charset matrix: every label must decode to the
// same UTF-8 text; unknown/bogus labels pass the bytes through unchanged
// (never an error).
func TestDecodeBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label string
		in    []byte
		want  string
	}{
		// 中文 in GBK.
		{"gbk", []byte{0xD6, 0xD0, 0xCE, 0xC4}, "中文"},
		// Same bytes, the GB2312 alias.
		{"GB2312", []byte{0xD6, 0xD0, 0xCE, 0xC4}, "中文"},
		// 中文 in Big5.
		{"big5", []byte{0xA4, 0xA4, 0xA4, 0xE5}, "中文"},
		// 日本語 in Shift_JIS.
		{"shift_jis", []byte{0x93, 0xFA, 0x96, 0x7B, 0x8C, 0xEA}, "日本語"},
		// Привет in windows-1251.
		{"windows-1251", []byte{0xCF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2}, "Привет"},
		// café in ISO-8859-1 (and its latin1 alias).
		{"iso-8859-1", []byte{'c', 'a', 'f', 0xE9}, "café"},
		{"latin1", []byte{'c', 'a', 'f', 0xE9}, "café"},
		// UTF-8 passes through (already the target).
		{"utf-8", []byte("héllo"), "héllo"},
		// UTF-7 is deliberately not in the WHATWG label set → passthrough.
		{"utf-7", []byte("plain +ACE-"), "plain +ACE-"},
		// Bogus and empty labels pass through unchanged.
		{"x-no-such-charset", []byte("raw bytes"), "raw bytes"},
		{"", []byte("raw bytes"), "raw bytes"},
		{"  ", []byte("raw bytes"), "raw bytes"},
	}
	for _, tc := range cases {
		got, err := io.ReadAll(DecodeBody(bytes.NewReader(tc.in), tc.label))
		if err != nil {
			t.Errorf("DecodeBody(%q): %v", tc.label, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("DecodeBody(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

// TestDecodeBodyNeverErrors hammers hostile labels; every one must yield a
// reader, never panic or refuse.
func TestDecodeBodyNeverErrors(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"utf-16", "UTF-16BE", "x-user-defined", `\"; DROP TABLE`, "\x00\x01", "a/b", "GBK "} {
		got, err := io.ReadAll(DecodeBody(bytes.NewReader([]byte("abc")), label))
		if err != nil {
			t.Errorf("DecodeBody(%q): %v", label, err)
			continue
		}
		if len(got) == 0 {
			t.Errorf("DecodeBody(%q): empty output", label)
		}
	}
}
