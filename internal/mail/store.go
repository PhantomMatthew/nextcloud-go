package mail

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SSL mode values for the imap_ssl_mode / smtp_ssl_mode columns, mirroring
// the official Mail app's enum.
const (
	SSLModeSSL      = "ssl"
	SSLModeStartTLS = "starttls"
	SSLModeNone     = "none"
)

// ValidSSLMode reports whether m is one of the three SSL mode enum values.
func ValidSSLMode(m string) bool {
	switch m {
	case SSLModeSSL, SSLModeStartTLS, SSLModeNone:
		return true
	}
	return false
}

// Account is one user's mail account row. PasswordSealed is always the
// sealed credential blob (ADR-0108 §3) — the store never sees plaintext.
type Account struct {
	ID             int64
	UserID         string
	Name           string
	Email          string
	IMAPHost       string
	IMAPPort       int
	IMAPSSLMode    string
	IMAPUser       string
	SMTPHost       string
	SMTPPort       int
	SMTPSSLMode    string
	SMTPUser       string
	PasswordSealed []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Mailbox is one synced IMAP mailbox row (mail_mailboxes, migration 0028).
// Name keeps the verbatim WIRE form (RFC 2152 modified UTF-7) so it
// round-trips into EXAMINE without re-encoding; the REST layer derives the
// display name with imap.DecodeMailboxName. LastSeenUID is the sync cursor:
// the highest UID the local copy has ever fetched (0 = never synced, which
// is also how the arrival event distinguishes the initial bulk sync).
type Mailbox struct {
	ID          int64
	AccountID   int64
	Name        string
	Delimiter   string
	UIDValidity int64
	UIDNext     int64
	LastSeenUID int64
	Selectable  bool
	SpecialUse  string
}

// MailboxCounts is a Mailbox plus its list-view counts for the REST API.
type MailboxCounts struct {
	Mailbox
	Total  int64
	Unread int64
}

// Message is one synced list-view summary row (mail_messages, 0028) — M3
// syncs the summaries, M4 serves them and fetches the bodies live. Flags
// keeps the storage-wrapped form (see PackFlags): tokens separated by
// single spaces with one leading and one trailing space, so the unread
// predicate stays token-exact (flags NOT LIKE '% Seen %' never matches a
// hypothetical "SeenX"). Preview/HasAttachments (0029) are the M6 sync-time
// list-view extras: the first 200 runes of the plain body with whitespace
// collapsed (” when the message had no plain part or exceeded the 1 MiB
// preview-fetch cap) and the paperclip flag.
type Message struct {
	ID             int64
	MailboxID      int64
	UID            int64
	MessageID      string
	Subject        string
	FromAddr       string
	ToAddrs        string
	DateUnix       int64
	Flags          string
	Size           int64
	Preview        string
	HasAttachments bool
}

// MessageHit is one unified-search result row (M6): the message plus the
// account id its API link needs.
type MessageHit struct {
	Message
	AccountID int64
}

// PackFlags renders flag tokens (already backslash-free: "Seen", "Flagged")
// into the storage form ' Seen Flagged ' — single-space separated with one
// leading and one trailing space. The empty set stores as ”.
func PackFlags(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	return " " + strings.Join(flags, " ") + " "
}

// Store persists mail accounts, mailboxes, and message summaries. Every
// account read/write is scoped to the owning user: a row another user owns
// is indistinguishable from a missing one. Mailbox/message methods key off
// the account/mailbox ids their callers resolved through the owning user.
type Store interface {
	Create(ctx context.Context, a *Account) error
	GetByID(ctx context.Context, userID string, id int64) (*Account, error)
	ListByUser(ctx context.Context, userID string) ([]Account, error)
	// ListAll returns every account of every user — the background sync job
	// (mail.sync) fans out over all users, unlike the per-user API paths.
	ListAll(ctx context.Context) ([]Account, error)
	Update(ctx context.Context, a *Account) error
	Delete(ctx context.Context, userID string, id int64) error

	// UpsertMailbox inserts the mailbox or refreshes its LIST-derived
	// columns (delimiter, selectable, special_use) when (account, name)
	// already exists; the sync cursors (uidvalidity/uidnext/last_seen_uid)
	// are never clobbered on update. m is repopulated from the row.
	UpsertMailbox(ctx context.Context, m *Mailbox) error
	ListMailboxes(ctx context.Context, accountID int64) ([]Mailbox, error)
	ListMailboxCounts(ctx context.Context, accountID int64) ([]MailboxCounts, error)
	// DeleteMailboxes removes the given mailbox rows and their messages
	// (vanished-from-server cleanup and the account cascade share this).
	DeleteMailboxes(ctx context.Context, ids []int64) error

	ListMessageUIDs(ctx context.Context, mailboxID int64) ([]int64, error)
	// RecentMessageUIDs returns up to limit uids, highest first (the flag
	// refresh window).
	RecentMessageUIDs(ctx context.Context, mailboxID int64, limit int) ([]int64, error)
	// InsertMessages batch-inserts summary rows; the caller chunks.
	InsertMessages(ctx context.Context, msgs []Message) error
	DeleteMessagesByUID(ctx context.Context, mailboxID int64, uids []int64) error
	// DeleteAllMessages wipes one mailbox's rows (UIDVALIDITY change).
	DeleteAllMessages(ctx context.Context, mailboxID int64) error
	UpdateMailboxSyncState(ctx context.Context, mailboxID, uidValidity, uidNext, lastSeenUID int64) error
	// SetMessageFlags rewrites one row's flags only when they changed.
	SetMessageFlags(ctx context.Context, mailboxID, uid int64, flags string) error

	// M4 (ADR-0108 §6): the message list/detail views and the local side of
	// the live delete/move ops. ListMessages keyset-paginates newest-first
	// (beforeDate == 0 selects the first page); GetMessage/GetMailbox scope
	// their row to the owning mailbox/account (ErrNotFound otherwise);
	// DeleteMessage lands the local half of a live delete or move (a moved
	// message is rediscovered in its destination by the next sync).
	ListMessages(ctx context.Context, mailboxID, beforeDate, beforeID int64, limit int) ([]Message, error)
	GetMessage(ctx context.Context, mailboxID, messageID int64) (*Message, error)
	GetMailbox(ctx context.Context, accountID, mailboxID int64) (*Mailbox, error)
	DeleteMessage(ctx context.Context, mailboxID, messageID int64) error

	// M6 (ADR-0108): SetMessagePreview fills one row's sync-time list
	// preview; SearchMessages joins the caller's messages across all their
	// accounts and mailboxes for the unified-search provider (subject and
	// from_addr, case-insensitive contains, newest first).
	SetMessagePreview(ctx context.Context, mailboxID, uid int64, preview string, hasAttachments bool) error
	SearchMessages(ctx context.Context, userID, term string, limit int) ([]MessageHit, error)
}

// ErrNotFound reports a missing (or not-owned) account row.
var ErrNotFound = errors.New("mail: account not found")

// ErrInvalid reports an account row missing its required fields.
var ErrInvalid = errors.New("mail: invalid account")

// joinPasswords packs the IMAP/SMTP password pair into the single
// password_sealed plaintext: be16(len(imap)) || imap || smtp. M1's schema
// carries one sealed column (ADR-0108 §3), so both credentials ride in it.
func joinPasswords(imap, smtp string) ([]byte, error) {
	if len(imap) > 0xffff {
		return nil, errors.New("mail: password too long")
	}
	out := make([]byte, 2, 2+len(imap)+len(smtp))
	//nolint:gosec // G115: length is bounds-checked against 0xffff above
	binary.BigEndian.PutUint16(out, uint16(len(imap)))
	out = append(out, imap...)
	out = append(out, smtp...)
	return out, nil
}

func splitPasswords(packed []byte) (imap, smtp string, err error) {
	if len(packed) < 2 {
		return "", "", fmt.Errorf("%w: packed passwords truncated", ErrCredential)
	}
	n := int(binary.BigEndian.Uint16(packed))
	if len(packed) < 2+n {
		return "", "", fmt.Errorf("%w: packed passwords truncated", ErrCredential)
	}
	return string(packed[2 : 2+n]), string(packed[2+n:]), nil
}
