package webdav

import (
	"context"
	"io"
	"slices"
	"strings"
	"time"
)

// WriteCond carries the conditional headers of a write request.
type WriteCond struct {
	IfETags     []string // ETags from the If: state lists — require current ETag ∈ set
	IfMatch     []string // If-Match list; "*" wildcard as an entry
	IfNoneMatch []string // If-None-Match list; "*" wildcard as an entry
}

// CondWriteFS is implemented by filesystems that evaluate write preconditions
// atomically with the write (ADR-0094).
type CondWriteFS interface {
	WriteIf(ctx context.Context, user, path string, r io.Reader, mtime *time.Time, cond *WriteCond) (*Entry, bool, error)
}

// ParseIfETags extracts every quoted entity-tag appearing inside the [...]
// state lists of an If: header (RFC 4918 §10.4): the plain form (["e1"]),
// uri-tagged lists (</remote.php/dav/files/u/f> (["e1"])), multiple
// parenthesized lists, and weak tags (W/ stripped so the tag can match stored
// etags). Parenthesized lists whose first token (case-insensitive) is Not are
// skipped — Nextcloud clients never send them. Lock tokens (<...>) live
// outside [...] and are naturally excluded. Malformed input yields whatever
// was extracted; ParseIfETags never errors.
func ParseIfETags(h string) []string {
	var out []string
	for i := 0; i < len(h); {
		open := strings.IndexByte(h[i:], '(')
		if open < 0 {
			break
		}
		start := i + open + 1
		end := strings.IndexByte(h[start:], ')')
		var list string
		if end < 0 {
			list = h[start:]
			i = len(h)
		} else {
			list = h[start : start+end]
			i = start + end + 1
		}
		if listStartsWithNot(list) {
			continue
		}
		out = append(out, bracketETags(list)...)
	}
	return out
}

// Evaluate checks the preconditions against the current target state and
// returns ErrPrecondition on the first failed rule. A nil receiver (no
// conditional headers) always passes.
func (c *WriteCond) Evaluate(exists bool, etag string) error {
	if c == nil {
		return nil
	}
	if len(c.IfETags) > 0 && (!exists || !slices.Contains(c.IfETags, etag)) {
		return ErrPrecondition
	}
	if slices.Contains(c.IfMatch, "*") && !exists {
		return ErrPrecondition
	}
	if len(c.IfMatch) > 0 && !slices.Contains(c.IfMatch, "*") && (!exists || !slices.Contains(c.IfMatch, etag)) {
		return ErrPrecondition
	}
	if slices.Contains(c.IfNoneMatch, "*") && exists {
		return ErrPrecondition
	}
	if len(c.IfNoneMatch) > 0 && exists && slices.Contains(c.IfNoneMatch, etag) {
		return ErrPrecondition
	}
	return nil
}

// splitETagList splits a comma-separated entity-tag list (If-Match /
// If-None-Match) into bare tags: quotes and W/ weakness prefixes are
// stripped, "*" passes through as an entry.
func splitETagList(h string) []string {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(h, ",") {
		v := strings.TrimSpace(raw)
		v = strings.TrimPrefix(v, "W/")
		v = strings.Trim(v, `"`)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// listStartsWithNot reports whether the first state-list token is Not.
func listStartsWithNot(list string) bool {
	t := strings.TrimSpace(list)
	if len(t) < 3 || !strings.EqualFold(t[:3], "not") {
		return false
	}
	if len(t) == 3 {
		return true
	}
	switch t[3] {
	case ' ', '\t', '<', '[', '"', '(':
		return true
	}
	return false
}

// bracketETags extracts the quoted contents of every [...] span in s; a W/
// prefix sits outside the quotes and is stripped implicitly.
func bracketETags(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		open := strings.IndexByte(s[i:], '[')
		if open < 0 {
			break
		}
		start := i + open + 1
		end := strings.IndexByte(s[start:], ']')
		var span string
		if end < 0 {
			span = s[start:]
			i = len(s)
		} else {
			span = s[start : start+end]
			i = start + end + 1
		}
		out = append(out, quotedTokens(span)...)
	}
	return out
}

// quotedTokens extracts every "..." token in s, ignoring unterminated
// fragments.
func quotedTokens(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		q := strings.IndexByte(s[i:], '"')
		if q < 0 {
			break
		}
		start := i + q + 1
		end := strings.IndexByte(s[start:], '"')
		if end < 0 {
			break
		}
		out = append(out, s[start:start+end])
		i = start + end + 1
	}
	return out
}
