package imap

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// mailbox.go carries the M3 sync commands (ADR-0108 §5): LIST, EXAMINE
// (read-only SELECT — sync must never mutate server state), UID SEARCH, and
// the summary/flags UID FETCH shapes. ENVELOPE subjects and address display
// names stay raw here (RFC 2047 encoded-words decode at the mail layer);
// flags lose their leading '\' so the store can keep query-friendly tokens
// ("Seen", "Flagged").

// internalDateLayout is the RFC 3501 date-time format (1-2 digit day).
const internalDateLayout = "2-Jan-2006 15:04:05 -0700"

// MailboxInfo is one LIST response entry. WireName keeps the verbatim wire
// form (modified UTF-7, quoting already unescaped) so it round-trips into
// EXAMINE through quoteString without any re-encoding; the display name is
// derived at the mail layer with DecodeMailboxName.
type MailboxInfo struct {
	WireName   string
	Delim      string // "" when the server answered NIL
	Attrs      []string
	Selectable bool // false when the \Noselect attribute is present
}

// SelectResult is the EXAMINE outcome the sync engine diffs against.
type SelectResult struct {
	Exists      uint64
	UIDValidity uint64
	UIDNext     uint64
}

// EnvelopeAddress is one RFC 3501 ENVELOPE address quad reduced to the
// fields the list view needs (the at-domain-list route is dropped).
type EnvelopeAddress struct {
	Name    string
	Mailbox string
	Host    string
}

// Envelope carries the ENVELOPE fields M3 syncs (list-view summaries).
type Envelope struct {
	Subject   string
	MessageID string
	From      []EnvelopeAddress
	To        []EnvelopeAddress
}

// MessageSummary is one UID FETCH (UID FLAGS INTERNALDATE RFC822.SIZE
// ENVELOPE) response row.
type MessageSummary struct {
	UID          uint64
	Flags        []string
	InternalDate time.Time
	Size         uint32
	Envelope     Envelope
}

// List issues LIST "" "*" and returns every reported mailbox. A malformed
// LIST line is a protocol violation and fails the client.
func (c *Client) List(ctx context.Context) ([]MailboxInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resp, err := c.roundTrip(`LIST "" "*"`)
	if err != nil {
		return nil, err
	}
	if resp.status != "OK" {
		return nil, c.fail(fmt.Errorf("imap: LIST: %s %s", resp.status, resp.text))
	}
	out := []MailboxInfo{}
	for _, line := range resp.untagged {
		if !strings.HasPrefix(line, "* LIST ") {
			continue
		}
		mb, err := parseListLine(line)
		if err != nil {
			return nil, c.fail(err)
		}
		out = append(out, mb)
	}
	return out, nil
}

// parseListLine parses "* LIST (attrs) delim name": attrs via the paren
// tokenizer, the delimiter quoted-or-NIL, and the name quoted-or-atom kept
// in wire form.
func parseListLine(line string) (MailboxInfo, error) {
	toks, err := parseTokens(strings.TrimPrefix(line, "* LIST "))
	if err != nil {
		return MailboxInfo{}, fmt.Errorf("imap: LIST line: %w", err)
	}
	if len(toks) != 3 {
		return MailboxInfo{}, fmt.Errorf("imap: LIST line with %d fields, want 3", len(toks))
	}
	attrList, ok := toks[0].([]any)
	if !ok {
		return MailboxInfo{}, fmt.Errorf("imap: LIST attributes not a list in %q", line)
	}
	name, ok := toks[2].(string)
	if !ok || name == "" {
		return MailboxInfo{}, fmt.Errorf("imap: LIST mailbox name missing in %q", line)
	}
	mb := MailboxInfo{WireName: name, Selectable: true}
	if delim, ok := toks[1].(string); ok {
		mb.Delim = delim
	}
	for _, a := range attrList {
		s, ok := a.(string)
		if !ok {
			continue
		}
		mb.Attrs = append(mb.Attrs, s)
		if strings.EqualFold(s, "\\Noselect") {
			mb.Selectable = false
		}
	}
	return mb, nil
}

// Examine issues EXAMINE (the read-only SELECT: sync must never mutate
// server state) for wireName and parses the untagged EXISTS and the
// UIDVALIDITY/UIDNEXT response codes. wireName passes through quoteString,
// so a crafted name cannot inject into the command line.
func (c *Client) Examine(ctx context.Context, wireName string) (SelectResult, error) {
	if err := ctx.Err(); err != nil {
		return SelectResult{}, err
	}
	q, err := quoteString(wireName)
	if err != nil {
		return SelectResult{}, err
	}
	resp, err := c.roundTrip("EXAMINE " + q)
	if err != nil {
		return SelectResult{}, err
	}
	if resp.status != "OK" {
		return SelectResult{}, c.fail(fmt.Errorf("imap: EXAMINE %q: %s %s", wireName, resp.status, resp.text))
	}
	var sr SelectResult
	for _, line := range resp.untagged {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "*" && strings.EqualFold(fields[2], "EXISTS") {
			n, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return SelectResult{}, c.fail(fmt.Errorf("imap: EXISTS count in %q: %w", line, err))
			}
			sr.Exists = n
			continue
		}
		code, value, ok := bracketedCode(line)
		if !ok {
			continue
		}
		var target *uint64
		switch strings.ToUpper(code) {
		case "UIDVALIDITY":
			target = &sr.UIDValidity
		case "UIDNEXT":
			target = &sr.UIDNext
		default:
			continue
		}
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return SelectResult{}, c.fail(fmt.Errorf("imap: %s in %q: %w", code, line, err))
		}
		*target = n
	}
	return sr, nil
}

// bracketedCode extracts the first [CODE value] response code of an untagged
// line ("* OK [UIDVALIDITY 42] text" → "UIDVALIDITY", "42"); codes without a
// value and lines without brackets report ok=false.
func bracketedCode(line string) (code, value string, ok bool) {
	open := strings.IndexByte(line, '[')
	if open < 0 {
		return "", "", false
	}
	closeIdx := strings.IndexByte(line[open:], ']')
	if closeIdx < 0 {
		return "", "", false
	}
	fields := strings.Fields(line[open+1 : open+closeIdx])
	if len(fields) < 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// UIDSearchAll issues UID SEARCH ALL and returns every matching UID.
func (c *Client) UIDSearchAll(ctx context.Context) ([]uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resp, err := c.roundTrip("UID SEARCH ALL")
	if err != nil {
		return nil, err
	}
	if resp.status != "OK" {
		return nil, c.fail(fmt.Errorf("imap: UID SEARCH: %s %s", resp.status, resp.text))
	}
	out := []uint64{}
	for _, line := range resp.untagged {
		rest, ok := strings.CutPrefix(line, "* SEARCH")
		if !ok {
			continue
		}
		for _, tok := range strings.Fields(rest) {
			n, err := strconv.ParseUint(tok, 10, 64)
			if err != nil {
				return nil, c.fail(fmt.Errorf("imap: SEARCH uid in %q: %w", line, err))
			}
			out = append(out, n)
		}
	}
	return out, nil
}

// UIDFetchSummaries fetches UID/FLAGS/INTERNALDATE/RFC822.SIZE/ENVELOPE for
// uids. Batching is the caller's job (the sync engine sends ≤500 per call).
func (c *Client) UIDFetchSummaries(ctx context.Context, uids []uint64) ([]MessageSummary, error) {
	lines, err := c.uidFetch(ctx, uids, "(UID FLAGS INTERNALDATE RFC822.SIZE ENVELOPE)")
	if err != nil {
		return nil, err
	}
	out := []MessageSummary{}
	for _, line := range lines {
		attrs, ok, err := fetchAttrList(line)
		if err != nil {
			return nil, c.fail(err)
		}
		if !ok {
			continue
		}
		m, err := attrMap(attrs)
		if err != nil {
			return nil, c.fail(err)
		}
		ms, ok, err := summaryFromAttrs(m)
		if err != nil {
			return nil, c.fail(err)
		}
		if !ok {
			continue // a UID FETCH data line without a UID is skipped, never fatal
		}
		out = append(out, ms)
	}
	return out, nil
}

// UIDFetchFlags fetches only UID/FLAGS for uids — the cheap poll-mode flag
// refresh (no CONDSTORE in v1).
func (c *Client) UIDFetchFlags(ctx context.Context, uids []uint64) (map[uint64][]string, error) {
	lines, err := c.uidFetch(ctx, uids, "(UID FLAGS)")
	if err != nil {
		return nil, err
	}
	out := map[uint64][]string{}
	for _, line := range lines {
		attrs, ok, err := fetchAttrList(line)
		if err != nil {
			return nil, c.fail(err)
		}
		if !ok {
			continue
		}
		m, err := attrMap(attrs)
		if err != nil {
			return nil, c.fail(err)
		}
		uid, ok := uintAttr(m, "UID")
		if !ok {
			continue
		}
		out[uid] = flagList(m["FLAGS"])
	}
	return out, nil
}

// uidFetch issues UID FETCH <uids> <items> and returns the untagged lines of
// the exchange. An empty uid set is a no-op (an empty set would be a
// malformed command).
func (c *Client) uidFetch(ctx context.Context, uids []uint64, items string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return nil, nil
	}
	var b strings.Builder
	for i, u := range uids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(u, 10))
	}
	resp, err := c.roundTrip("UID FETCH " + b.String() + " " + items)
	if err != nil {
		return nil, err
	}
	if resp.status != "OK" {
		return nil, c.fail(fmt.Errorf("imap: UID FETCH: %s %s", resp.status, resp.text))
	}
	return resp.untagged, nil
}

// fetchAttrList extracts the parenthesized attribute list of one
// "* <seq> FETCH (...)" untagged line; ok=false marks a non-FETCH line.
func fetchAttrList(line string) (attrs []any, ok bool, err error) {
	rest, ok := strings.CutPrefix(line, "* ")
	if !ok {
		return nil, false, nil
	}
	_, rest, ok = strings.Cut(rest, " ")
	if !ok {
		return nil, false, nil
	}
	body, ok := strings.CutPrefix(rest, "FETCH")
	if !ok {
		return nil, false, nil
	}
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "(") {
		return nil, false, fmt.Errorf("imap: malformed FETCH data line %q", line)
	}
	toks, err := parseTokens(body)
	if err != nil {
		return nil, false, fmt.Errorf("imap: FETCH data: %w", err)
	}
	if len(toks) != 1 {
		return nil, false, fmt.Errorf("imap: FETCH data line with %d top-level tokens", len(toks))
	}
	list, ok := toks[0].([]any)
	if !ok {
		return nil, false, fmt.Errorf("imap: FETCH data not a parenthesized list in %q", line)
	}
	return list, true, nil
}

// attrMap folds the alternating key/value attribute list into a map keyed by
// the upper-cased attribute name.
func attrMap(attrs []any) (map[string]any, error) {
	m := make(map[string]any, len(attrs)/2)
	for i := 0; i < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if !ok || i+1 >= len(attrs) {
			return nil, fmt.Errorf("imap: FETCH attribute list is not key/value pairs")
		}
		m[strings.ToUpper(key)] = attrs[i+1]
	}
	return m, nil
}

// summaryFromAttrs builds a MessageSummary from one FETCH attribute map;
// ok=false when the mandatory UID is absent.
func summaryFromAttrs(m map[string]any) (MessageSummary, bool, error) {
	var ms MessageSummary
	uid, ok := uintAttr(m, "UID")
	if !ok {
		return MessageSummary{}, false, nil
	}
	ms.UID = uid
	ms.Flags = flagList(m["FLAGS"])
	if v, ok := m["INTERNALDATE"].(string); ok {
		t, err := time.Parse(internalDateLayout, v)
		if err != nil {
			return MessageSummary{}, false, fmt.Errorf("imap: INTERNALDATE %q: %w", v, err)
		}
		ms.InternalDate = t
	}
	if v, ok := m["RFC822.SIZE"].(string); ok {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return MessageSummary{}, false, fmt.Errorf("imap: RFC822.SIZE %q: %w", v, err)
		}
		if n > math.MaxUint32 {
			// A >4GiB message must not wedge sync; the list view does not
			// need the exact octet count.
			n = math.MaxUint32
		}
		ms.Size = uint32(n) //nolint:gosec // G115: clamped to MaxUint32 above
	}
	if v, present := m["ENVELOPE"]; present {
		ms.Envelope = parseEnvelope(v)
	}
	return ms, true, nil
}

// uintAttr reads a numeric atom attribute; ok=false when absent or unparsable.
func uintAttr(m map[string]any, key string) (uint64, bool) {
	v, ok := m[key].(string)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// flagList reduces a FLAGS list to its atoms with the leading '\' stripped,
// so the store keeps query-friendly tokens ("Seen", "Flagged").
func flagList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, f := range list {
		if s, ok := f.(string); ok {
			out = append(out, strings.TrimPrefix(s, "\\"))
		}
	}
	return out
}

// parseEnvelope reduces the RFC 3501 ENVELOPE list —
// (date subject from sender reply-to to cc bcc in-reply-to message-id) —
// to the summary fields. Anything malformed yields zero values, never a
// panic: one broken message must not fail the batch.
func parseEnvelope(v any) Envelope {
	list, ok := v.([]any)
	if !ok {
		return Envelope{}
	}
	var env Envelope
	str := func(i int) string {
		if i < len(list) {
			if s, ok := list[i].(string); ok {
				return s
			}
		}
		return ""
	}
	env.Subject = str(1)
	env.From = parseAddressList(at(list, 2))
	env.To = parseAddressList(at(list, 5))
	env.MessageID = str(9)
	return env
}

// at returns list[i] or nil, index-safe.
func at(list []any, i int) any {
	if i < len(list) {
		return list[i]
	}
	return nil
}

// parseAddressList reduces one ENVELOPE address list to its valid quads:
// entries that are not (name adl mailbox host), and group syntax markers
// (NIL mailbox or host), are skipped — never a panic.
func parseAddressList(v any) []EnvelopeAddress {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := []EnvelopeAddress{}
	for _, item := range list {
		quad, ok := item.([]any)
		if !ok || len(quad) < 4 {
			continue
		}
		name, nameOK := quad[0].(string)
		mailbox, mbOK := quad[2].(string)
		host, hostOK := quad[3].(string)
		if !mbOK || !hostOK || mailbox == "" || host == "" {
			continue
		}
		if !nameOK {
			name = ""
		}
		out = append(out, EnvelopeAddress{Name: name, Mailbox: mailbox, Host: host})
	}
	return out
}
