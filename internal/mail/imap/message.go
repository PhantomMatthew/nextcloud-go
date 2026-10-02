package imap

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// message.go carries the M4 live-op commands (ADR-0108 §6): SELECT (the
// read-write EXAMINE sibling), whole-message UID FETCH (BODY.PEEK[]), and
// the write commands UID STORE / UID COPY / EXPUNGE. The sync engine (M3)
// stays strictly read-only; these exist for the per-request message APIs.

var (
	// ErrCommandRefused reports a tagged NO to an M4 command: the mailbox or
	// message is gone, or the server refused the operation. Callers classify
	// with errors.Is (the mail layer maps it to a 404). Like every command
	// failure it also breaks the client — the M4 ops dial per request and
	// close after the exchange, so a broken client is never reused.
	ErrCommandRefused = errors.New("imap: command refused (NO)")
	// ErrInvalidFlag reports a UID STORE flag outside the M4 allowlist; the
	// command is rejected before anything is written (injection guard), and
	// the client stays usable.
	ErrInvalidFlag = errors.New("imap: flag outside the allowlist")
)

// The M4 flag allowlist: system flags (\Seen, optionally without the
// backslash at this layer — the caller sends the canonical backslash form)
// or keywords ($Label / plain atoms). Anything else — whitespace, quotes,
// list breakouts, CR/LF — is rejected before any write.
var (
	systemFlagRe = regexp.MustCompile(`^\\?(Seen|Answered|Flagged|Deleted|Draft|Recent)$`)
	keywordRe    = regexp.MustCompile(`^\$?[A-Za-z0-9_]+$`)
)

// allowedFlag reports whether f is in the M4 UID STORE allowlist.
func allowedFlag(f string) bool {
	return systemFlagRe.MatchString(f) || keywordRe.MatchString(f)
}

// uidSet renders uids comma-joined for the wire; ok=false for an empty set
// (an empty set would be a malformed command, so it is a no-op).
func uidSet(uids []uint64) (string, bool) {
	if len(uids) == 0 {
		return "", false
	}
	var b strings.Builder
	for i, u := range uids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(u, 10))
	}
	return b.String(), true
}

// UIDFetchFull fetches the whole RFC 822 message for uid via
// UID FETCH <uid> (UID BODY.PEEK[]) — PEEK, so the fetch itself never sets
// \Seen. The M2 literal-capable reader assembles the BODY[] literal into the
// logical line (a literal over maxLiteralSize already errors there), and the
// tokenizer yields it as a string. A FETCH data line naming a different UID
// is skipped (a read-write mailbox may legitimately send unsolicited updates
// for other messages). When the server answers OK but returns no data for
// the uid — the message is gone — the result is (nil, nil).
func (c *Client) UIDFetchFull(ctx context.Context, uid uint64) ([]byte, error) {
	lines, err := c.uidFetch(ctx, []uint64{uid}, "(UID BODY.PEEK[])")
	if err != nil {
		return nil, err
	}
	var found bool
	var body []byte
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
		got, ok := uintAttr(m, "UID")
		if !ok || got != uid {
			continue // unsolicited data for another message (or no UID at all)
		}
		v, ok := m["BODY[]"].(string)
		if !ok {
			return nil, c.fail(fmt.Errorf("imap: UID FETCH BODY[] missing or not a literal in %q", line))
		}
		found = true
		body = []byte(v)
	}
	if !found {
		return nil, nil
	}
	return body, nil
}

// UIDStore applies flag deltas: UID STORE <set> +FLAGS.SILENT (<add>) then
// -FLAGS.SILENT (<remove>); an empty side is skipped, and both empty is a
// no-op. SILENT keeps the wire quiet — the caller already knows the new
// state. Every flag is allowlist-validated before ANY write (injection
// guard): a rejected flag is ErrInvalidFlag and the client stays usable. A
// tagged NO is ErrCommandRefused.
func (c *Client) UIDStore(ctx context.Context, uids []uint64, add, remove []string) error {
	for _, f := range add {
		if !allowedFlag(f) {
			return fmt.Errorf("%w: %q", ErrInvalidFlag, f)
		}
	}
	for _, f := range remove {
		if !allowedFlag(f) {
			return fmt.Errorf("%w: %q", ErrInvalidFlag, f)
		}
	}
	set, ok := uidSet(uids)
	if !ok {
		return nil
	}
	store := func(op string, flags []string) error {
		resp, err := c.roundTrip("UID STORE " + set + " " + op + "FLAGS.SILENT (" + strings.Join(flags, " ") + ")")
		if err != nil {
			return err
		}
		switch resp.status {
		case "OK":
			return nil
		case "NO":
			return c.fail(withText(ErrCommandRefused, resp.text))
		default:
			return c.fail(fmt.Errorf("imap: UID STORE: %s %s", resp.status, resp.text))
		}
	}
	if len(add) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := store("+", add); err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := store("-", remove); err != nil {
			return err
		}
	}
	return nil
}

// UIDCopy copies uids to the destination mailbox (wire name through
// quoteString, so a crafted name cannot inject). A tagged NO is
// ErrCommandRefused.
func (c *Client) UIDCopy(ctx context.Context, uids []uint64, destWireName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	set, ok := uidSet(uids)
	if !ok {
		return nil
	}
	q, err := quoteString(destWireName)
	if err != nil {
		return err
	}
	resp, err := c.roundTrip("UID COPY " + set + " " + q)
	if err != nil {
		return err
	}
	switch resp.status {
	case "OK":
		return nil
	case "NO":
		return c.fail(withText(ErrCommandRefused, resp.text))
	default:
		return c.fail(fmt.Errorf("imap: UID COPY: %s %s", resp.status, resp.text))
	}
}

// Expunge permanently removes every \Deleted message of the selected
// mailbox; the untagged EXPUNGE lines are discarded. A tagged NO is
// ErrCommandRefused.
func (c *Client) Expunge(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resp, err := c.roundTrip("EXPUNGE")
	if err != nil {
		return err
	}
	switch resp.status {
	case "OK":
		return nil
	case "NO":
		return c.fail(withText(ErrCommandRefused, resp.text))
	default:
		return c.fail(fmt.Errorf("imap: EXPUNGE: %s %s", resp.status, resp.text))
	}
}
