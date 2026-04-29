package goldentest

import (
	"net/http"
	"sort"
	"strings"
)

func Normalize(c *Case, parsed *ParsedResponse) (*ParsedResponse, error) {
	if parsed == nil {
		return nil, ErrNotImplemented
	}
	out := &ParsedResponse{
		Status:  parsed.Status,
		Headers: parsed.Headers.Clone(),
		Body:    append([]byte(nil), parsed.Body...),
	}
	for _, rule := range c.Response.Normalize {
		applyHeaderRules(out.Headers, rule)
	}
	return out, nil
}

func applyHeaderRules(h http.Header, rule NormalizeRule) {
	for _, name := range rule.DropHeaders {
		h.Del(name)
	}
	if rule.ReplaceHeader != nil {
		h.Set(rule.ReplaceHeader.Name, rule.ReplaceHeader.With)
	}
}

func CanonicalHeaders(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		vals := append([]string(nil), h.Values(k)...)
		sort.Strings(vals)
		out = append(out, k+": "+strings.Join(vals, ", "))
	}
	return out
}
