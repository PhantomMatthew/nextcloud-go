package imap

import "testing"

// TestDecodeMailboxName covers the RFC 2152 modified UTF-7 decoder: pure
// ASCII passthrough, the "&-" escape, CJK/surrogate-pair base64 segments,
// mixed runs, and the malformed matrix (which must return the input
// unchanged and never panic).
func TestDecodeMailboxName(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"INBOX", "INBOX"},
		{"Sent Items", "Sent Items"},
		{"a&-b", "a&b"},
		{"Gesendet &- Co", "Gesendet & Co"},
		{"&ZeVnLIqe-", "日本語"},
		{"&U,BTFw-", "台北"},
		{"&XfJT0ZAB-", "已发送"},
		{"&V4NXPnux-", "垃圾箱"},
		{"Entw&APw-rfe", "Entwürfe"},
		{"&g0l6Pw-", "草稿"},
		{"&2DzfiQ-party", "🎉party"},
		{"&AMU-ngstr&APY-m", "Ångström"},
		{"INBOX.&ZeVnLIqe-", "INBOX.日本語"},
		{"&-&ZeVnLIqe-&-", "&日本語&"},
		// Malformed: unchanged input, no panic.
		{"&", "&"},                     // unterminated
		{"&ABC", "&ABC"},               // unterminated
		{"&!!!-", "&!!!-"},             // invalid base64
		{"&Z-", "&Z-"},                 // truncated base64 group
		{"&QQ-", "&QQ-"},               // odd byte count after decode
		{"&ZeVnLIqe", "&ZeVnLIqe"},     // missing terminator
		{"100% & more", "100% & more"}, // bare '&' without terminator
	}
	for _, tc := range cases {
		if got := DecodeMailboxName(tc.in); got != tc.want {
			t.Errorf("DecodeMailboxName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
