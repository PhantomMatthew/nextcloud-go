package imap

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// parse.go is the response-data tokenizer the M2 CAPABILITY path (and
// later ENVELOPE/BODYSTRUCTURE parsing) builds on. It produces a token tree
// of: atoms as string, quoted strings as string (with \" and \\ unescaped),
// literals as string (the reader has already absorbed their bytes into the
// logical line), NIL as nil, and parenthesized lists as []any. Malformed
// input is always an error, never a panic.

var (
	errUnbalancedQuote  = errors.New("imap: unbalanced quoted string")
	errUnbalancedParen  = errors.New("imap: unbalanced parenthesized list")
	errTruncatedEscape  = errors.New("imap: quoted string ends in an escape")
	errBadEscape        = errors.New("imap: quoted string escapes a non-special")
	errControlInQuoted  = errors.New("imap: CR or LF inside a quoted string")
	errBadLiteral       = errors.New("imap: malformed literal")
	errUnexpectedByte   = errors.New("imap: unexpected byte in response data")
	errTrailingClosePar = errors.New("imap: ')' without a matching '('")
)

// parseTokens splits response data into its token tree (see the file
// comment for the shapes). Top-level input is a space-separated token
// sequence, e.g. the rest of a "* CAPABILITY ..." line.
func parseTokens(s string) ([]any, error) {
	p := &parser{s: s}
	var out []any
	for {
		p.skipSpace()
		if p.eof() {
			return out, nil
		}
		if p.s[p.pos] == ')' {
			return nil, errTrailingClosePar
		}
		v, err := p.token()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

type parser struct {
	s   string
	pos int
}

func (p *parser) eof() bool { return p.pos >= len(p.s) }

func (p *parser) skipSpace() {
	for !p.eof() && p.s[p.pos] == ' ' {
		p.pos++
	}
}

func (p *parser) token() (any, error) {
	switch p.s[p.pos] {
	case '(':
		return p.list()
	case '"':
		return p.quoted()
	case '{':
		return p.literal()
	default:
		return p.atom()
	}
}

// list consumes a parenthesized sequence into a []any; the empty list is a
// non-nil []any{} so it stays distinguishable from a NIL atom.
func (p *parser) list() ([]any, error) {
	p.pos++ // consume '('
	out := []any{}
	for {
		p.skipSpace()
		if p.eof() {
			return nil, errUnbalancedParen
		}
		if p.s[p.pos] == ')' {
			p.pos++
			return out, nil
		}
		v, err := p.token()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

// quoted consumes a quoted string, unescaping \" and \\ (the only
// quoted-specials RFC 3501 admits). A bare CR/LF or an unknown escape is a
// protocol violation.
func (p *parser) quoted() (string, error) {
	p.pos++ // consume '"'
	var b strings.Builder
	for !p.eof() {
		c := p.s[p.pos]
		p.pos++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.eof() {
				return "", errTruncatedEscape
			}
			e := p.s[p.pos]
			p.pos++
			if e != '"' && e != '\\' {
				return "", fmt.Errorf("%w: %q", errBadEscape, e)
			}
			b.WriteByte(e)
		case '\r', '\n':
			return "", errControlInQuoted
		default:
			b.WriteByte(c)
		}
	}
	return "", errUnbalancedQuote
}

// literal consumes "{n}\r\n" plus exactly n octets into a string; the reader
// guarantees the bytes are present in the logical line.
func (p *parser) literal() (string, error) {
	p.pos++ // consume '{'
	start := p.pos
	for !p.eof() && p.s[p.pos] != '}' {
		p.pos++
	}
	if p.eof() {
		return "", errBadLiteral
	}
	digits := strings.TrimSuffix(p.s[start:p.pos], "+")
	p.pos++ // consume '}'
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return "", fmt.Errorf("%w: bad octet count", errBadLiteral)
	}
	if len(p.s)-p.pos < 2 || p.s[p.pos] != '\r' || p.s[p.pos+1] != '\n' {
		return "", fmt.Errorf("%w: marker not followed by CRLF", errBadLiteral)
	}
	p.pos += 2
	if len(p.s)-p.pos < n {
		return "", fmt.Errorf("%w: truncated octets", errBadLiteral)
	}
	lit := p.s[p.pos : p.pos+n]
	p.pos += n
	return lit, nil
}

// atom consumes a run of atom characters (anything but space, the
// parenthesis/quote/brace delimiters, and controls). NIL folds to nil.
func (p *parser) atom() (any, error) {
	start := p.pos
	for !p.eof() && isAtomByte(p.s[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return nil, fmt.Errorf("%w: %q", errUnexpectedByte, p.s[p.pos])
	}
	if a := p.s[start:p.pos]; !strings.EqualFold(a, "NIL") {
		return a, nil
	}
	return nil, nil
}

func isAtomByte(c byte) bool {
	switch c {
	case ' ', '(', ')', '"', '{', '}':
		return false
	}
	return c > 0x20 && c < 0x7f
}
