package mail

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// syncFake is a stateful scripted IMAP4rev1 server on a loopback listener
// for the M3 sync tests: beyond the M2 greeting/CAPABILITY/LOGIN/LOGOUT it
// answers LIST, EXAMINE, UID SEARCH ALL, and UID FETCH (summary and flags
// shapes) from an in-memory mailbox model the test mutates between sync
// runs. M4 added the live-op surface the message API tests drive: SELECT,
// UID STORE, UID COPY, EXPUNGE, and whole-message UID FETCH (BODY.PEEK[]),
// with an ops log (opsLog) so tests can assert the exact command sequence
// that crossed the wire. M5 added APPEND (classic continuation) for the
// save-to-Sent flow. M6 added the sync-time preview fetch: whole-message
// fetches are recorded separately (fullFetchLog) and only land in the ops
// log when the mailbox was SELECTed read-write — the ops log pins LIVE-OP
// sequences, and the sync's EXAMINE-session previews must not pollute them.
// The dialIMAP closure is the Syncer.DialIMAP, MessageOps.DialIMAP, and
// Sender.DialIMAP seam: it forces ssl mode "none" and re-aims every dial at
// the listener.
type syncFake struct {
	ln net.Listener
	wg sync.WaitGroup

	mu          sync.Mutex
	boxes       []*fakeMailbox
	ops         []string // M4: the live-op commands, in wire order
	fullFetches []string // M6: every UID FETCH BODY.PEEK[] uid set, any session
	failList    bool     // LIST answers NO (account-level failure injection)
	failApnd    bool     // M5: APPEND answers NO (save-to-Sent failure injection)
	failFull    bool     // M6: UID FETCH BODY.PEEK[] answers NO (preview failure injection)
}

type fakeMailbox struct {
	name        string // wire name
	attrs       []string
	delim       string // "" renders NIL
	selectable  bool
	uidvalidity int64
	msgs        []*fakeMsg
	fail        bool // EXAMINE/SELECT answer NO (per-mailbox failure isolation)
}

type fakeMsg struct {
	uid      int64
	flags    []string // wire atoms, backslash included: `\Seen`
	date     string   // INTERNALDATE wire form
	size     int
	subject  string
	fromName string
	fromAddr string // "mailbox@host"
	toNames  []string
	toAddrs  []string
	msgID    string
	raw      []byte // whole RFC 822 message for BODY.PEEK[] (nil = the server lost it)
}

func newSyncFake(t *testing.T) *syncFake {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &syncFake{ln: ln}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.wg.Wait()
	})
	return f
}

// dialIMAP is the DialIMAP seam for this fake (service and syncer alike).
func (f *syncFake) dialIMAP(ctx context.Context, opts imap.DialOptions) (*imap.Client, error) {
	opts.SSLMode = SSLModeNone
	opts.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, f.ln.Addr().String())
	}
	return imap.Dial(ctx, opts)
}

func (f *syncFake) mailbox(name string) *fakeMailbox {
	for _, mb := range f.boxes {
		if mb.name == name {
			return mb
		}
	}
	return nil
}

func (f *syncFake) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	_, _ = io.WriteString(conn, "* OK fake IMAP4rev1 ready\r\n")
	var selected *fakeMailbox
	selectedRW := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		verb, args, _ := strings.Cut(rest, " ")
		f.mu.Lock()
		switch verb {
		case "CAPABILITY":
			_, _ = io.WriteString(conn, "* CAPABILITY IMAP4rev1\r\n"+tag+" OK capability done\r\n")
		case "LOGIN":
			_, _ = io.WriteString(conn, tag+" OK logged in\r\n")
		case "LIST":
			if f.failList {
				_, _ = io.WriteString(conn, tag+" NO list broken\r\n")
				break
			}
			for _, mb := range f.boxes {
				attrs := append([]string(nil), mb.attrs...)
				if !mb.selectable {
					attrs = append(attrs, "\\Noselect")
				}
				delim := "NIL"
				if mb.delim != "" {
					delim = iq(mb.delim)
				}
				_, _ = io.WriteString(conn, "* LIST ("+strings.Join(attrs, " ")+") "+delim+" "+iq(mb.name)+"\r\n")
			}
			_, _ = io.WriteString(conn, tag+" OK list done\r\n")
		case "EXAMINE":
			name := unquote(args)
			mb := f.mailbox(name)
			switch {
			case mb == nil:
				_, _ = io.WriteString(conn, tag+" NO no such mailbox\r\n")
			case mb.fail:
				_, _ = io.WriteString(conn, tag+" NO mailbox broken\r\n")
			default:
				selected = mb
				selectedRW = false
				var b strings.Builder
				fmt.Fprintf(&b, "* %d EXISTS\r\n", len(mb.msgs))
				fmt.Fprintf(&b, "* OK [UIDVALIDITY %d] UIDs valid\r\n", mb.uidvalidity)
				fmt.Fprintf(&b, "* OK [UIDNEXT %d] Predicted next UID\r\n", mb.uidnext())
				b.WriteString(tag + " OK [READ-ONLY] examine done\r\n")
				_, _ = io.WriteString(conn, b.String())
			}
		case "SELECT":
			name := unquote(args)
			mb := f.mailbox(name)
			switch {
			case mb == nil:
				_, _ = io.WriteString(conn, tag+" NO no such mailbox\r\n")
			case mb.fail:
				_, _ = io.WriteString(conn, tag+" NO mailbox broken\r\n")
			default:
				selected = mb
				selectedRW = true
				f.ops = append(f.ops, "SELECT "+name)
				var b strings.Builder
				fmt.Fprintf(&b, "* %d EXISTS\r\n", len(mb.msgs))
				fmt.Fprintf(&b, "* OK [UIDVALIDITY %d] UIDs valid\r\n", mb.uidvalidity)
				fmt.Fprintf(&b, "* OK [UIDNEXT %d] Predicted next UID\r\n", mb.uidnext())
				b.WriteString(tag + " OK [READ-WRITE] select done\r\n")
				_, _ = io.WriteString(conn, b.String())
			}
		case "EXPUNGE":
			if selected == nil {
				_, _ = io.WriteString(conn, tag+" BAD no mailbox selected\r\n")
				break
			}
			f.ops = append(f.ops, "EXPUNGE")
			var b strings.Builder
			kept := selected.msgs[:0]
			seq := 0
			for _, m := range selected.msgs {
				seq++
				if hasWireFlag(m, "\\Deleted") {
					fmt.Fprintf(&b, "* %d EXPUNGE\r\n", seq)
					continue
				}
				kept = append(kept, m)
			}
			selected.msgs = kept
			b.WriteString(tag + " OK expunge done\r\n")
			_, _ = io.WriteString(conn, b.String())
		case "UID":
			f.serveUID(conn, tag, args, selected, selectedRW)
		case "APPEND":
			if !f.serveAppend(conn, r, tag, args) {
				f.mu.Unlock()
				return
			}
		case "LOGOUT":
			_, _ = io.WriteString(conn, "* BYE bye\r\n"+tag+" OK logout done\r\n")
			f.mu.Unlock()
			return
		default:
			_, _ = io.WriteString(conn, tag+" BAD unsupported\r\n")
		}
		f.mu.Unlock()
	}
}

// serveAppend answers the M5 APPEND command: `APPEND "name" (flags) "date"
// {n}` — the fake plays the classic continuation ("+ go ahead", then reads
// the literal and its CRLF) and files the payload as a new message of the
// target mailbox. failApnd injects a tagged NO (no continuation, no data).
// ok=false means the connection died mid-literal.
func (f *syncFake) serveAppend(conn net.Conn, r *bufio.Reader, tag, args string) bool {
	name := appendMailboxName(args)
	mb := f.mailbox(name)
	switch {
	case mb == nil:
		_, _ = io.WriteString(conn, tag+" NO no such mailbox\r\n")
	case f.failApnd:
		f.ops = append(f.ops, "APPEND "+name+" (refused)")
		_, _ = io.WriteString(conn, tag+" NO append broken\r\n")
	default:
		f.ops = append(f.ops, "APPEND "+name)
		n := appendLiteralSize(args)
		_, _ = io.WriteString(conn, "+ go ahead\r\n")
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return false
		}
		var crlf [2]byte
		if _, err := io.ReadFull(r, crlf[:]); err != nil {
			return false
		}
		flags := ""
		if open := strings.Index(args, "("); open >= 0 {
			if closing := strings.Index(args[open:], ")"); closing > 0 {
				flags = args[open+1 : open+closing]
			}
		}
		mb.msgs = append(mb.msgs, &fakeMsg{
			uid:   mb.uidnext(),
			flags: strings.Fields(flags),
			raw:   payload,
			size:  len(payload),
			date:  "2-Jan-2006 15:04:05 +0000",
		})
		_, _ = io.WriteString(conn, tag+" OK append done\r\n")
	}
	return true
}

// appendMailboxName extracts the quoted mailbox name that opens an APPEND
// command line (`"Sent" (\Seen) "date" {n}` — test names carry no escapes).
func appendMailboxName(args string) string {
	rest, ok := strings.CutPrefix(args, `"`)
	if !ok {
		return args
	}
	name, _, _ := strings.Cut(rest, `"`)
	return name
}

// appendLiteralSize parses the {n} literal marker at the end of an APPEND
// command line.
func appendLiteralSize(args string) int {
	open := strings.LastIndexByte(args, '{')
	closing := strings.LastIndexByte(args, '}')
	if open < 0 || closing < open {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(args[open+1:closing], "+"))
	return n
}

// serveUID answers UID SEARCH ALL, UID FETCH (summaries, flags, and the
// whole-message BODY.PEEK[] form), UID STORE, and UID COPY for the selected
// mailbox. selectedRW tells EXAMINE (sync) and SELECT (live-op) sessions
// apart — only the latter's traffic feeds the ops log.
func (f *syncFake) serveUID(conn net.Conn, tag, args string, selected *fakeMailbox, selectedRW bool) {
	sub, rest, _ := strings.Cut(args, " ")
	if selected == nil {
		_, _ = io.WriteString(conn, tag+" BAD no mailbox selected\r\n")
		return
	}
	switch sub {
	case "SEARCH":
		var b strings.Builder
		b.WriteString("* SEARCH")
		for _, m := range selected.msgs {
			fmt.Fprintf(&b, " %d", m.uid)
		}
		b.WriteString("\r\n" + tag + " OK search done\r\n")
		_, _ = io.WriteString(conn, b.String())
	case "FETCH":
		set, items, _ := strings.Cut(rest, " (")
		items = strings.TrimSuffix(items, ")")
		if strings.Contains(items, "BODY.PEEK[]") {
			f.serveFetchFull(conn, tag, set, selected, selectedRW)
			return
		}
		var b strings.Builder
		seq := 0
		for _, m := range selected.msgs {
			if !uidInSet(m.uid, set) {
				continue
			}
			seq++
			fmt.Fprintf(&b, "* %d FETCH (%s)\r\n", seq, m.fetchAttrs(items))
		}
		b.WriteString(tag + " OK fetch done\r\n")
		_, _ = io.WriteString(conn, b.String())
	case "STORE":
		// STORE <set> +/-FLAGS.SILENT (<flags>)
		set, opAndFlags, _ := strings.Cut(rest, " ")
		op, flagsStr, _ := strings.Cut(opAndFlags, " ")
		flagsStr = strings.TrimPrefix(strings.TrimSuffix(flagsStr, ")"), "(")
		f.ops = append(f.ops, "UID STORE "+set+" "+op+" ("+flagsStr+")")
		add := strings.HasPrefix(op, "+")
		for _, m := range selected.msgs {
			if !uidInSet(m.uid, set) {
				continue
			}
			for _, fl := range strings.Fields(flagsStr) {
				if add {
					if !hasWireFlag(m, fl) {
						m.flags = append(m.flags, fl)
					}
				} else {
					m.flags = dropWireFlag(m.flags, fl)
				}
			}
		}
		_, _ = io.WriteString(conn, tag+" OK store done\r\n")
	case "COPY":
		set, destQ, _ := strings.Cut(rest, " ")
		dest := unquote(destQ)
		f.ops = append(f.ops, "UID COPY "+set+" "+dest)
		target := f.mailbox(dest)
		if target == nil {
			_, _ = io.WriteString(conn, tag+" NO no such mailbox\r\n")
			return
		}
		for _, m := range selected.msgs {
			if !uidInSet(m.uid, set) {
				continue
			}
			dup := *m
			dup.uid = target.uidnext()
			target.msgs = append(target.msgs, &dup)
		}
		_, _ = io.WriteString(conn, tag+" OK copy done\r\n")
	default:
		_, _ = io.WriteString(conn, tag+" BAD unsupported uid command\r\n")
	}
}

// serveFetchFull answers UID FETCH <set> (UID BODY.PEEK[]): each matching
// message whose raw bytes exist is one literal-bearing FETCH line; a
// message without raw bytes is simply not returned (the message-gone case).
// failFull injects a tagged NO. Every request is recorded in fullFetches;
// only a read-write (SELECT) session's request also joins the live-op log —
// the M6 sync previews run over EXAMINE and would otherwise break the ops
// sequence assertions.
func (f *syncFake) serveFetchFull(conn net.Conn, tag, set string, selected *fakeMailbox, selectedRW bool) {
	f.fullFetches = append(f.fullFetches, set)
	if f.failFull {
		_, _ = io.WriteString(conn, tag+" NO fetch broken\r\n")
		return
	}
	if selectedRW {
		f.ops = append(f.ops, "UID FETCH "+set+" (UID BODY.PEEK[])")
	}
	var buf bytes.Buffer
	seq := 0
	for _, m := range selected.msgs {
		if !uidInSet(m.uid, set) || m.raw == nil {
			continue
		}
		seq++
		fmt.Fprintf(&buf, "* %d FETCH (UID %d BODY[] {%d}\r\n", seq, m.uid, len(m.raw))
		buf.Write(m.raw)
		buf.WriteString(")\r\n")
	}
	buf.WriteString(tag + " OK fetch done\r\n")
	_, _ = conn.Write(buf.Bytes())
}

// opsLog returns the live-op commands the fake saw, in wire order.
func (f *syncFake) opsLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// fullFetchLog returns the uid sets of every UID FETCH BODY.PEEK[] the fake
// saw, in wire order — M6 sync previews included (they never join opsLog).
func (f *syncFake) fullFetchLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fullFetches...)
}

// setFailApnd flips the M5 APPEND-refusal knob mid-test (mu-guarded — a
// serve goroutine may be reading it on another connection).
func (f *syncFake) setFailApnd(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failApnd = v
}

// setFailFull flips the M6 full-fetch-refusal knob mid-test (mu-guarded).
func (f *syncFake) setFailFull(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failFull = v
}

// hasWireFlag reports whether the message carries the flag (case-folded —
// IMAP flags are case-insensitive atoms).
func hasWireFlag(m *fakeMsg, flag string) bool {
	for _, fl := range m.flags {
		if strings.EqualFold(fl, flag) {
			return true
		}
	}
	return false
}

// dropWireFlag removes every case-folded match of flag.
func dropWireFlag(flags []string, flag string) []string {
	out := flags[:0]
	for _, fl := range flags {
		if !strings.EqualFold(fl, flag) {
			out = append(out, fl)
		}
	}
	return out
}

// uidnext reports max uid + 1 (0 for an empty mailbox, matching nothing —
// the tests never depend on the empty case's exact value).
func (mb *fakeMailbox) uidnext() int64 {
	var maxUID int64
	for _, m := range mb.msgs {
		if m.uid > maxUID {
			maxUID = m.uid
		}
	}
	return maxUID + 1
}

// fetchAttrs renders the requested FETCH data items for the message.
func (m *fakeMsg) fetchAttrs(items string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "UID %d", m.uid)
	for _, item := range strings.Fields(items) {
		switch item {
		case "FLAGS":
			fmt.Fprintf(&b, " FLAGS (%s)", strings.Join(m.flags, " "))
		case "INTERNALDATE":
			fmt.Fprintf(&b, " INTERNALDATE %s", iq(m.date))
		case "RFC822.SIZE":
			fmt.Fprintf(&b, " RFC822.SIZE %d", m.size)
		case "ENVELOPE":
			b.WriteString(" ENVELOPE " + m.envelope())
		}
	}
	return b.String()
}

// envelope renders the RFC 3501 ENVELOPE list:
// (date subject from sender reply-to to cc bcc in-reply-to message-id).
func (m *fakeMsg) envelope() string {
	from := "NIL"
	if m.fromAddr != "" {
		mb, host, _ := strings.Cut(m.fromAddr, "@")
		from = "((" + quotedOrNil(m.fromName) + " NIL " + iq(mb) + " " + iq(host) + "))"
	}
	to := "NIL"
	if len(m.toAddrs) > 0 {
		parts := make([]string, 0, len(m.toAddrs))
		for i, addr := range m.toAddrs {
			name := "NIL"
			if i < len(m.toNames) {
				name = quotedOrNil(m.toNames[i])
			}
			mb, host, _ := strings.Cut(addr, "@")
			parts = append(parts, "("+name+" NIL "+iq(mb)+" "+iq(host)+")")
		}
		to = "(" + strings.Join(parts, " ") + ")"
	}
	return "(" + quotedOrNil(m.date) + " " + quotedOrNil(m.subject) + " " + from + " NIL NIL " + to + " NIL NIL NIL " + quotedOrNil(m.msgID) + ")" //nolint:dupword // the wire shape carries legitimate NIL runs
}

// quotedOrNil renders NIL for the empty string, a quoted string otherwise.
func quotedOrNil(s string) string {
	if s == "" {
		return "NIL"
	}
	return iq(s)
}

// iq renders an IMAP quoted string; test data carries no quote or backslash
// characters, so no escaping is needed (and strconv.Quote's Go escapes
// would corrupt non-ASCII on the wire).
func iq(s string) string { return `"` + s + `"` }

// uidInSet reports whether uid appears in the comma-separated uid set.
func uidInSet(uid int64, set string) bool {
	for _, tok := range strings.Split(set, ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64); err == nil && n == uid {
			return true
		}
	}
	return false
}

// unquote strips one layer of double quotes (test names carry no escapes).
func unquote(s string) string {
	return strings.Trim(s, `"`)
}
