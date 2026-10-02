package imap

import (
	"reflect"
	"testing"
)

func TestParseTokens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want []any
	}{
		{"empty", "", nil},
		{"atoms", "IMAP4rev1 STARTTLS", []any{"IMAP4rev1", "STARTTLS"}},
		{"quoted keeps spaces", `"hello world" x`, []any{"hello world", "x"}},
		{"quoted unescapes", `"a\"b\\c"`, []any{`a"b\c`}},
		{"empty quoted", `""`, []any{""}},
		{"nil folds", "NIL nil", []any{nil, nil}},
		{"flat list", "(a b c)", []any{[]any{"a", "b", "c"}}},
		{"empty list", "()", []any{[]any{}}},
		{"nested lists", "(a (b c) (d (e)))", []any{[]any{"a", []any{"b", "c"}, []any{"d", []any{"e"}}}}},
		{"list with nil and quoted", `(NIL "x y" 1)`, []any{[]any{nil, "x y", "1"}}},
		{"literal", "{5}\r\nhello", []any{"hello"}},
		{"literal with specials", "{8}\r\na\"()b\r\nc", []any{"a\"()b\r\nc"}},
		{"literal in list", "({3}\r\nabc x)", []any{[]any{"abc", "x"}}},
		{"literal plus form", "{4+}\r\nwxyz", []any{"wxyz"}},
		{"envelope-ish", "(\"NIL\" NIL ((\"a\" NIL \"b\" \"c\")) {3}\r\ndef)", []any{[]any{"NIL", nil, []any{[]any{"a", nil, "b", "c"}}, "def"}}},
		{"section atom keeps brackets", "BODY[HEADER] 1", []any{"BODY[HEADER]", "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTokens(tc.in)
			if err != nil {
				t.Fatalf("parseTokens(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseTokens(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseTokensMalformed is the fuzz-ish table: every input must error
// cleanly, never panic and never desync into a half-tree.
func TestParseTokensMalformed(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{
		`"unterminated`,
		`(a b`,
		`((a)`,
		`a)`,
		`)`,
		`"bad\q escape"`,
		`"trailing escape\`,
		"\"cr\rlf\"",
		"\"lf\nhere\"",
		`{x}`,
		`{5}` + "\n" + `abcde`,
		"{5}\r\nab",
		`{`,
		`{3`,
		"{}",
	} {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			if got, err := parseTokens(tc); err == nil {
				t.Errorf("parseTokens(%q) = %#v, want error", tc, got)
			}
		})
	}
}
