package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"sort"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"

	"github.com/vmihailenco/msgpack/v5"
)

// sync.go is the M3 sync engine (ADR-0108 §5): poll-only, UID-incremental,
// read-only on the server (EXAMINE, never SELECT; no STORE). Per account it
// upserts the mailbox list from LIST, drops mailboxes/messages that vanished
// from the server, fetches new list-view summaries in batches, refreshes the
// flags of the most recent window (cheap convergence without CONDSTORE), and
// publishes mail.message.arrived for INBOX arrivals (never for the initial
// bulk sync). M6 adds the insert-time list previews: the newest
// syncPreviewWindow new messages up to syncPreviewMaxSize are fetched whole
// (BODY.PEEK[] — still read-only) so the list view gains a snippet and the
// has-attachments flag. Per-mailbox failures are logged and skipped;
// account-level failures (credentials, dial, login, LIST) abort the
// account. The Syncer holds no per-account mutable state, so accounts are
// safe to sync sequentially or concurrently; one account is synced by one
// caller at a time (the job fan-out is sequential, and the REST endpoint +
// job could overlap only as a last-writer-wins cursor update, which the
// next run repairs through the uid diff).

const (
	// syncFetchBatch is the UID FETCH batch size the wire stays under.
	syncFetchBatch = 500
	// syncMaxNewPerRun caps newly fetched uids per mailbox per run; the
	// remainder resumes next run through the local/server uid diff.
	syncMaxNewPerRun = 2000
	// syncFlagWindow is how many recent uids get their flags refreshed.
	syncFlagWindow = 200
	// syncPreviewWindow is how many of the newest newly inserted messages
	// get a list preview per mailbox per run.
	syncPreviewWindow = 50
	// syncPreviewMaxSize caps the full-message fetch previews need; a bigger
	// message keeps the empty preview (documented limitation — the list
	// view shows no snippet and no paperclip for it).
	syncPreviewMaxSize = 1 << 20
)

// EventMessageArrived is the bus topic published when a sync finds new
// messages in an account's INBOX after the initial bulk sync; the payload
// is msgpack (see Arrival).
const EventMessageArrived = "mail.message.arrived"

// Arrival is the mail.message.arrived payload: how many messages arrived,
// and the newest one's sender/subject for the bell notification.
type Arrival struct {
	UserID        string `msgpack:"userID"`
	AccountID     int64  `msgpack:"accountID"`
	MailboxID     int64  `msgpack:"mailboxID"`
	Count         int    `msgpack:"count"`
	LatestFrom    string `msgpack:"latestFrom"`
	LatestSubject string `msgpack:"latestSubject"`
}

// DecodeArrival unpacks one mail.message.arrived payload.
func DecodeArrival(payload []byte) (Arrival, error) {
	var a Arrival
	if err := msgpack.Unmarshal(payload, &a); err != nil {
		return Arrival{}, fmt.Errorf("mail: decode arrival: %w", err)
	}
	return a, nil
}

// Syncer syncs one account's mailboxes and list-view summaries. Store is
// the mail Store (accounts + mailboxes + messages); Secret is the instance
// secret the credentials seal under; DialIMAP is the dial seam (nil →
// imap.Dial, production installs the egress-guarded closure); Logger and
// Bus are optional (nil-safe). Passwords are never logged.
type Syncer struct {
	Store    Store
	Secret   string
	DialIMAP func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error)
	Logger   *slog.Logger
	Bus      *events.Bus
}

// SyncAccount runs one full sync pass over the account and returns how many
// new messages were inserted. The caller owns the ctx deadline (the job
// arms 2 minutes per account; the REST endpoint uses the request ctx).
func (s *Syncer) SyncAccount(ctx context.Context, a *Account) (int, error) {
	imapPW, _, err := openPasswords(s.Secret, a.UserID, a.IMAPHost, a.IMAPUser, a.PasswordSealed)
	if err != nil {
		return 0, err
	}
	dial := s.DialIMAP
	if dial == nil {
		dial = imap.Dial
	}
	client, err := dial(ctx, imap.DialOptions{Host: a.IMAPHost, Port: a.IMAPPort, SSLMode: a.IMAPSSLMode})
	if err != nil {
		return 0, fmt.Errorf("mail: sync: dial: %w", err)
	}
	if err := client.Login(a.IMAPUser, imapPW); err != nil {
		return 0, errors.Join(fmt.Errorf("mail: sync: login: %w", err), client.Logout())
	}
	defer func() {
		// LOGOUT is best-effort (ADR-0108 §1): a rude reply cannot un-sync.
		if err := client.Logout(); err != nil && s.Logger != nil {
			s.Logger.DebugContext(ctx, "mail: sync: logout failed", slog.String("error", err.Error()))
		}
	}()

	boxes, err := client.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("mail: sync: list: %w", err)
	}
	local, err := s.Store.ListMailboxes(ctx, a.ID)
	if err != nil {
		return 0, err
	}
	onServer := make(map[string]bool, len(boxes))
	syncable := make([]*Mailbox, 0, len(boxes))
	for i := range boxes {
		info := &boxes[i]
		onServer[info.WireName] = true
		mb := &Mailbox{
			AccountID:  a.ID,
			Name:       info.WireName,
			Delimiter:  info.Delim,
			Selectable: info.Selectable,
			SpecialUse: specialUse(info.Attrs),
		}
		if err := s.Store.UpsertMailbox(ctx, mb); err != nil {
			return 0, err
		}
		if mb.Selectable {
			syncable = append(syncable, mb)
		}
	}
	// Mailboxes the server no longer reports lose their rows and messages.
	var vanished []int64
	for i := range local {
		if !onServer[local[i].Name] {
			vanished = append(vanished, local[i].ID)
		}
	}
	if err := s.Store.DeleteMailboxes(ctx, vanished); err != nil {
		return 0, err
	}

	total := 0
	for _, mb := range syncable {
		n, err := s.syncMailbox(ctx, client, a, mb)
		if err != nil {
			// Per-mailbox failure isolation: one broken mailbox must not
			// starve the others; the next run retries it.
			s.warn(ctx, "mail: sync: mailbox failed", a, mb.Name, err)
			continue
		}
		total += n
	}
	return total, nil
}

// syncMailbox syncs one selectable mailbox: EXAMINE, UIDVALIDITY wipe on
// change, uid diff (vanish delete + capped new fetch), cursor advance, flag
// refresh window, and the INBOX arrival event. It returns the number of
// newly inserted messages.
func (s *Syncer) syncMailbox(ctx context.Context, client *imap.Client, a *Account, mb *Mailbox) (int, error) {
	prevLastSeen := mb.LastSeenUID
	sr, err := client.Examine(ctx, mb.Name)
	if err != nil {
		return 0, err
	}
	//nolint:gosec // G115: UIDVALIDITY is a non-negative 32-bit value
	wiped := int64(sr.UIDValidity) != mb.UIDValidity
	if wiped {
		// The server's uid space was reborn: every local row is stale.
		if err := s.Store.DeleteAllMessages(ctx, mb.ID); err != nil {
			return 0, err
		}
		mb.LastSeenUID = 0
	}
	serverUIDs, err := client.UIDSearchAll(ctx)
	if err != nil {
		return 0, err
	}
	localUIDs, err := s.Store.ListMessageUIDs(ctx, mb.ID)
	if err != nil {
		return 0, err
	}
	vanished, fresh := diffUIDs(localUIDs, int64s(serverUIDs))
	if err := s.Store.DeleteMessagesByUID(ctx, mb.ID, vanished); err != nil {
		return 0, err
	}
	if len(fresh) > syncMaxNewPerRun {
		fresh = fresh[:syncMaxNewPerRun]
	}
	newMsgs, latest, err := s.fetchNew(ctx, client, mb.ID, fresh)
	if err != nil {
		return 0, err
	}
	inserted := len(newMsgs)
	lastSeen := mb.LastSeenUID
	if latest != nil && latest.UID > lastSeen {
		lastSeen = latest.UID
	}
	//nolint:gosec // G115: UIDVALIDITY/UIDNEXT are non-negative 32-bit values
	if err := s.Store.UpdateMailboxSyncState(ctx, mb.ID, int64(sr.UIDValidity), int64(sr.UIDNext), lastSeen); err != nil {
		return inserted, err
	}
	if inserted > 0 && prevLastSeen > 0 && !wiped && strings.EqualFold(mb.Name, "INBOX") {
		s.publishArrived(ctx, a, mb, inserted, latest)
	}
	if err := s.refreshFlags(ctx, client, mb.ID); err != nil {
		return inserted, err
	}
	// M6: list previews ride the same connection, last of all — a preview
	// fetch failure breaks the client, and it must not take the cursor
	// advance or the flag refresh down with it.
	s.fetchPreviews(ctx, client, mb.ID, newMsgs)
	return inserted, nil
}

// fetchNew fetches and inserts summaries for the fresh uids in
// syncFetchBatch batches, returning the inserted rows and the highest-uid
// one.
func (s *Syncer) fetchNew(ctx context.Context, client *imap.Client, mailboxID int64, fresh []int64) ([]Message, *Message, error) {
	var inserted []Message
	var latest *Message
	for start := 0; start < len(fresh); start += syncFetchBatch {
		end := min(start+syncFetchBatch, len(fresh))
		sums, err := client.UIDFetchSummaries(ctx, uint64s(fresh[start:end]))
		if err != nil {
			return inserted, nil, err
		}
		msgs := make([]Message, 0, len(sums))
		for i := range sums {
			m := summaryToMessage(mailboxID, &sums[i])
			msgs = append(msgs, m)
			if latest == nil || m.UID > latest.UID {
				latest = &msgs[len(msgs)-1]
			}
		}
		if err := s.Store.InsertMessages(ctx, msgs); err != nil {
			return inserted, nil, err
		}
		inserted = append(inserted, msgs...)
	}
	return inserted, latest, nil
}

// fetchPreviews fills the list preview of the newest syncPreviewWindow
// newly inserted messages whose synced size fits syncPreviewMaxSize, over
// the same already-logged-in connection the sync holds: each message is
// fetched whole (UID FETCH BODY.PEEK[] — PEEK never sets \Seen) and parsed,
// and its row updated. Every fetch/parse failure skips its message with a
// debug log — a preview must never fail the sync — and an oversized message
// keeps the empty preview (documented limitation). Messages outside the
// window stay empty too; previews are an insert-time extra, not a
// backfill.
func (s *Syncer) fetchPreviews(ctx context.Context, client *imap.Client, mailboxID int64, msgs []Message) {
	if len(msgs) == 0 {
		return
	}
	byUID := make([]Message, len(msgs))
	copy(byUID, msgs)
	sort.Slice(byUID, func(i, j int) bool { return byUID[i].UID > byUID[j].UID })
	if len(byUID) > syncPreviewWindow {
		byUID = byUID[:syncPreviewWindow]
	}
	for i := range byUID {
		m := &byUID[i]
		if m.Size > syncPreviewMaxSize {
			continue
		}
		//nolint:gosec // G115: uids are non-negative 32-bit values
		raw, err := client.UIDFetchFull(ctx, uint64(m.UID))
		if err != nil {
			s.debug(ctx, "mail: sync: preview fetch failed", mailboxID, m.UID, err)
			continue
		}
		if raw == nil {
			continue // the message vanished between the summary and the full fetch
		}
		parsed, err := ParseMessage(raw)
		if err != nil {
			s.debug(ctx, "mail: sync: preview parse failed", mailboxID, m.UID, err)
			continue
		}
		if err := s.Store.SetMessagePreview(ctx, mailboxID, m.UID, previewText(parsed.TextPlain), len(parsed.Attachments) > 0); err != nil {
			s.debug(ctx, "mail: sync: preview store failed", mailboxID, m.UID, err)
		}
	}
}

// refreshFlags re-fetches the flags of the most recent local uids and
// rewrites the rows that changed — the poll-mode convergence that stands in
// for CONDSTORE in v1.
func (s *Syncer) refreshFlags(ctx context.Context, client *imap.Client, mailboxID int64) error {
	recent, err := s.Store.RecentMessageUIDs(ctx, mailboxID, syncFlagWindow)
	if err != nil || len(recent) == 0 {
		return err
	}
	flags, err := client.UIDFetchFlags(ctx, uint64s(recent))
	if err != nil {
		return err
	}
	for uid, fl := range flags {
		//nolint:gosec // G115: uids are non-negative 32-bit values
		if err := s.Store.SetMessageFlags(ctx, mailboxID, int64(uid), PackFlags(fl)); err != nil {
			return err
		}
	}
	return nil
}

// publishArrived emits mail.message.arrived; emission never fails the sync.
func (s *Syncer) publishArrived(ctx context.Context, a *Account, mb *Mailbox, count int, latest *Message) {
	if s.Bus == nil {
		return
	}
	payload, err := msgpack.Marshal(map[string]any{
		"userID":        a.UserID,
		"accountID":     a.ID,
		"mailboxID":     mb.ID,
		"count":         count,
		"latestFrom":    latest.FromAddr,
		"latestSubject": latest.Subject,
	})
	if err != nil {
		return
	}
	s.Bus.Publish(ctx, events.Event{Topic: EventMessageArrived, Payload: payload, Source: "host", UserID: a.UserID})
}

// diffUIDs splits local vs server uid lists: vanished are local uids the
// server lost, fresh are server uids never synced, sorted ascending.
func diffUIDs(local, server []int64) (vanished, fresh []int64) {
	onServer := make(map[int64]bool, len(server))
	for _, u := range server {
		onServer[u] = true
	}
	known := make(map[int64]bool, len(local))
	for _, u := range local {
		known[u] = true
		if !onServer[u] {
			vanished = append(vanished, u)
		}
	}
	for _, u := range server {
		if !known[u] {
			fresh = append(fresh, u)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i] < fresh[j] })
	return vanished, fresh
}

// uint64s converts non-negative uid ints for the wire.
func uint64s(uids []int64) []uint64 {
	out := make([]uint64, len(uids))
	for i, u := range uids {
		out[i] = uint64(u) //nolint:gosec // G115: uids are non-negative 32-bit values
	}
	return out
}

// int64s converts wire uids for the store.
func int64s(uids []uint64) []int64 {
	out := make([]int64, len(uids))
	for i, u := range uids {
		out[i] = int64(u) //nolint:gosec // G115: uids are non-negative 32-bit values
	}
	return out
}

// summaryToMessage maps a wire summary to its row: RFC 2047 encoded-words
// in the subject and display names decode here (the mail layer, per
// ADR-0108 §1 the imap package stays wire-level), the message-id loses its
// angle brackets, and flags take the storage form (PackFlags).
func summaryToMessage(mailboxID int64, sm *imap.MessageSummary) Message {
	var dec mime.WordDecoder
	return Message{
		MailboxID: mailboxID,
		UID:       int64(sm.UID), //nolint:gosec // G115: uids are non-negative 32-bit values
		MessageID: strings.Trim(sm.Envelope.MessageID, "<>"),
		Subject:   decodeHeader(&dec, sm.Envelope.Subject),
		FromAddr:  formatAddresses(&dec, sm.Envelope.From),
		ToAddrs:   formatAddresses(&dec, sm.Envelope.To),
		DateUnix:  sm.InternalDate.Unix(),
		Flags:     PackFlags(sm.Flags),
		Size:      int64(sm.Size),
	}
}

// decodeHeader decodes RFC 2047 encoded-words; malformed input stays raw
// (one bad header must not lose the message).
func decodeHeader(dec *mime.WordDecoder, s string) string {
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// formatAddresses renders an ENVELOPE address list comma-joined, each
// entry "Name <mailbox@host>" or bare "mailbox@host".
func formatAddresses(dec *mime.WordDecoder, addrs []imap.EnvelopeAddress) string {
	out := make([]string, 0, len(addrs))
	for _, e := range addrs {
		addr := e.Mailbox + "@" + e.Host
		if name := strings.TrimSpace(decodeHeader(dec, e.Name)); name != "" {
			out = append(out, name+" <"+addr+">")
			continue
		}
		out = append(out, addr)
	}
	return strings.Join(out, ", ")
}

// specialUse maps the LIST attributes to the first recognized special-use
// token (lowercase, no backslash), else "".
func specialUse(attrs []string) string {
	for _, want := range []struct{ attr, use string }{
		{"\\Sent", "sent"},
		{"\\Trash", "trash"},
		{"\\Drafts", "drafts"},
		{"\\Junk", "junk"},
		{"\\Archive", "archive"},
		{"\\Flagged", "flagged"},
		{"\\All", "all"},
	} {
		for _, a := range attrs {
			if strings.EqualFold(a, want.attr) {
				return want.use
			}
		}
	}
	return ""
}

func (s *Syncer) warn(ctx context.Context, msg string, a *Account, mailbox string, err error) {
	if s.Logger == nil {
		return
	}
	s.Logger.WarnContext(ctx, msg,
		slog.String("user", a.UserID), slog.Int64("account", a.ID), slog.String("mailbox", mailbox), slog.String("error", err.Error()))
}

func (s *Syncer) debug(ctx context.Context, msg string, mailboxID, uid int64, err error) {
	if s.Logger == nil {
		return
	}
	s.Logger.DebugContext(ctx, msg,
		slog.Int64("mailbox", mailboxID), slog.Int64("uid", uid), slog.String("error", err.Error()))
}
