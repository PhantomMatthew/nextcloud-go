package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// syncstore.go is the M3 mailbox/message half of SQLStore (migration 0028,
// ADR-0108 §5): mailbox upsert/list/counts for the REST API and the sync
// engine's cursor + summary persistence, plus the M4 message queries
// (ADR-0108 §6): keyset-paginated list views, single-row reads, and the
// local row deletes the live delete/move ops land, plus M6: the sync-time
// list-preview write and the unified-search join. The sync engine and the
// M4 ops write; the REST API reads and resolves scope.

// inChunk bounds one IN (...) placeholder list so even the oldest sqlite
// variable limit stays far away.
const inChunk = 400

const mailboxColumns = `id, account_id, name, delimiter, uidvalidity, uidnext, last_seen_uid, selectable, special_use`

func (s *SQLStore) ListAll(ctx context.Context) ([]Account, error) {
	rows, err := s.db.Query(ctx, `
SELECT `+accountColumns+`
FROM mail_accounts ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("mail: list all accounts: %w", err)
	}
	defer rows.Close()
	out := make([]Account, 0)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail: list all accounts: %w", err)
	}
	return out, nil
}

func (s *SQLStore) UpsertMailbox(ctx context.Context, m *Mailbox) error {
	if m == nil || m.AccountID <= 0 || m.Name == "" {
		return fmt.Errorf("%w: mailbox", ErrInvalid)
	}
	if m.Delimiter == "" {
		m.Delimiter = "/"
	}
	res, err := s.db.Exec(ctx, `
UPDATE mail_mailboxes SET delimiter = ?, selectable = ?, special_use = ?
WHERE account_id = ? AND name = ?`, m.Delimiter, m.Selectable, m.SpecialUse, m.AccountID, m.Name)
	if err != nil {
		return fmt.Errorf("mail: upsert mailbox: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mail: upsert mailbox: %w", err)
	}
	if n == 0 {
		if _, err := s.db.Exec(ctx, `
INSERT INTO mail_mailboxes (account_id, name, delimiter, uidvalidity, uidnext, last_seen_uid, selectable, special_use)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			m.AccountID, m.Name, m.Delimiter, m.UIDValidity, m.UIDNext, m.LastSeenUID, m.Selectable, m.SpecialUse); err != nil {
			return fmt.Errorf("mail: insert mailbox: %w", err)
		}
	}
	got, err := scanMailbox(s.db.QueryRow(ctx, `
SELECT `+mailboxColumns+`
FROM mail_mailboxes WHERE account_id = ? AND name = ?`, m.AccountID, m.Name))
	if err != nil {
		return err
	}
	*m = *got
	return nil
}

func (s *SQLStore) ListMailboxes(ctx context.Context, accountID int64) ([]Mailbox, error) {
	rows, err := s.db.Query(ctx, `
SELECT `+mailboxColumns+`
FROM mail_mailboxes WHERE account_id = ? ORDER BY id ASC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: list mailboxes: %w", err)
	}
	defer rows.Close()
	out := make([]Mailbox, 0)
	for rows.Next() {
		m, err := scanMailbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail: list mailboxes: %w", err)
	}
	return out, nil
}

// ListMailboxCounts joins each mailbox with its message counts. The unread
// predicate is token-exact by construction: flags stores ' Seen Flagged '
// (PackFlags), so '% Seen %' cannot match a longer token.
func (s *SQLStore) ListMailboxCounts(ctx context.Context, accountID int64) ([]MailboxCounts, error) {
	rows, err := s.db.Query(ctx, `
SELECT m.id, m.account_id, m.name, m.delimiter, m.uidvalidity, m.uidnext, m.last_seen_uid, m.selectable, m.special_use,
       COUNT(msg.id), COALESCE(SUM(CASE WHEN msg.flags NOT LIKE '% Seen %' THEN 1 ELSE 0 END), 0)
FROM mail_mailboxes m LEFT JOIN mail_messages msg ON msg.mailbox_id = m.id
WHERE m.account_id = ?
GROUP BY m.id
ORDER BY m.id ASC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: list mailbox counts: %w", err)
	}
	defer rows.Close()
	out := make([]MailboxCounts, 0)
	for rows.Next() {
		var mc MailboxCounts
		if err := rows.Scan(&mc.ID, &mc.AccountID, &mc.Name, &mc.Delimiter, &mc.UIDValidity, &mc.UIDNext,
			&mc.LastSeenUID, &mc.Selectable, &mc.SpecialUse, &mc.Total, &mc.Unread); err != nil {
			return nil, fmt.Errorf("mail: scan mailbox counts: %w", err)
		}
		out = append(out, mc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail: list mailbox counts: %w", err)
	}
	return out, nil
}

// DeleteMailboxes removes the given mailboxes and all their messages in one
// transaction. An empty id list is a no-op.
func (s *SQLStore) DeleteMailboxes(ctx context.Context, ids []int64) (err error) {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mail: delete mailboxes: begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rerr := tx.Rollback(); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	for start := 0; start < len(ids); start += inChunk {
		chunk := ids[start:min(start+inChunk, len(ids))]
		ph := placeholders(len(chunk))
		if _, err := tx.Exec(ctx, `DELETE FROM mail_messages WHERE mailbox_id IN (`+ph+`)`, int64Args(chunk)...); err != nil {
			return fmt.Errorf("mail: delete mailboxes: messages: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mail_mailboxes WHERE id IN (`+ph+`)`, int64Args(chunk)...); err != nil {
			return fmt.Errorf("mail: delete mailboxes: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mail: delete mailboxes: commit: %w", err)
	}
	committed = true
	return nil
}

func (s *SQLStore) ListMessageUIDs(ctx context.Context, mailboxID int64) ([]int64, error) {
	return s.queryUIDs(ctx, `
SELECT uid FROM mail_messages WHERE mailbox_id = ? ORDER BY uid ASC`, mailboxID)
}

// RecentMessageUIDs returns up to limit uids, highest first — the flag
// refresh window.
func (s *SQLStore) RecentMessageUIDs(ctx context.Context, mailboxID int64, limit int) ([]int64, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.queryUIDs(ctx, `
SELECT uid FROM mail_messages WHERE mailbox_id = ? ORDER BY uid DESC LIMIT ?`, mailboxID, limit)
}

func (s *SQLStore) queryUIDs(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("mail: query message uids: %w", err)
	}
	defer rows.Close()
	out := make([]int64, 0)
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("mail: scan message uid: %w", err)
		}
		out = append(out, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail: query message uids: %w", err)
	}
	return out, nil
}

const messageColumns = `mailbox_id, uid, message_id, subject, from_addr, to_addrs, date_unix, flags, size`

// insertChunk bounds one multi-row INSERT's placeholder count.
const insertChunk = 250

func (s *SQLStore) InsertMessages(ctx context.Context, msgs []Message) error {
	for start := 0; start < len(msgs); start += insertChunk {
		end := min(start+insertChunk, len(msgs))
		chunk := msgs[start:end]
		var b strings.Builder
		b.WriteString(`INSERT INTO mail_messages (` + messageColumns + `) VALUES `)
		args := make([]any, 0, len(chunk)*9)
		for i := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			m := &chunk[i]
			args = append(args, m.MailboxID, m.UID, m.MessageID, m.Subject, m.FromAddr, m.ToAddrs, m.DateUnix, m.Flags, m.Size)
		}
		if _, err := s.db.Exec(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("mail: insert messages: %w", err)
		}
	}
	return nil
}

func (s *SQLStore) DeleteMessagesByUID(ctx context.Context, mailboxID int64, uids []int64) error {
	for start := 0; start < len(uids); start += inChunk {
		chunk := uids[start:min(start+inChunk, len(uids))]
		args := append([]any{mailboxID}, int64Args(chunk)...)
		if _, err := s.db.Exec(ctx,
			`DELETE FROM mail_messages WHERE mailbox_id = ? AND uid IN (`+placeholders(len(chunk))+`)`, args...); err != nil {
			return fmt.Errorf("mail: delete messages by uid: %w", err)
		}
	}
	return nil
}

// DeleteAllMessages wipes one mailbox's rows (UIDVALIDITY change).
func (s *SQLStore) DeleteAllMessages(ctx context.Context, mailboxID int64) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM mail_messages WHERE mailbox_id = ?`, mailboxID); err != nil {
		return fmt.Errorf("mail: delete all messages: %w", err)
	}
	return nil
}

func (s *SQLStore) UpdateMailboxSyncState(ctx context.Context, mailboxID, uidValidity, uidNext, lastSeenUID int64) error {
	res, err := s.db.Exec(ctx, `
UPDATE mail_mailboxes SET uidvalidity = ?, uidnext = ?, last_seen_uid = ? WHERE id = ?`,
		uidValidity, uidNext, lastSeenUID, mailboxID)
	if err != nil {
		return fmt.Errorf("mail: update mailbox sync state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mail: update mailbox sync state: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: mailbox %d", ErrNotFound, mailboxID)
	}
	return nil
}

// SetMessageFlags rewrites one row's flags only when they actually changed,
// so the poll-mode refresh costs nothing on a quiet mailbox.
func (s *SQLStore) SetMessageFlags(ctx context.Context, mailboxID, uid int64, flags string) error {
	if _, err := s.db.Exec(ctx, `
UPDATE mail_messages SET flags = ? WHERE mailbox_id = ? AND uid = ? AND flags <> ?`,
		flags, mailboxID, uid, flags); err != nil {
		return fmt.Errorf("mail: set message flags: %w", err)
	}
	return nil
}

// messageRowColumns is messageColumns plus the primary key and the M6
// list-preview columns, for the M4 single-row and list-view reads.
const messageRowColumns = `id, ` + messageColumns + `, preview, has_attachments`

// ListMessages returns one page of the list view, newest first: rows with
// (date_unix, id) strictly BEFORE the cursor, keyset-paginated so same-date
// ties walk by id and a sync inserting rows mid-walk never shifts the page
// boundaries. beforeDate == 0 means the first page (no cursor). limit <= 0
// returns nothing (the handler clamps its query param).
func (s *SQLStore) ListMessages(ctx context.Context, mailboxID, beforeDate, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	if beforeDate == 0 {
		beforeDate = math.MaxInt64
		beforeID = math.MaxInt64
	}
	rows, err := s.db.Query(ctx, `
SELECT `+messageRowColumns+`
FROM mail_messages
WHERE mailbox_id = ? AND (date_unix < ? OR (date_unix = ? AND id < ?))
ORDER BY date_unix DESC, id DESC LIMIT ?`, mailboxID, beforeDate, beforeDate, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("mail: list messages: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail: list messages: %w", err)
	}
	return out, nil
}

// GetMessage reads one message row scoped to its mailbox; a missing (or
// other-mailbox) row is ErrNotFound.
func (s *SQLStore) GetMessage(ctx context.Context, mailboxID, messageID int64) (*Message, error) {
	return scanMessage(s.db.QueryRow(ctx, `
SELECT `+messageRowColumns+`
FROM mail_messages WHERE mailbox_id = ? AND id = ?`, mailboxID, messageID))
}

// GetMailbox reads one mailbox row scoped to its account; a missing (or
// other-account) row is ErrNotFound — the M4 handlers resolve the
// {account, mailbox} scope through it.
func (s *SQLStore) GetMailbox(ctx context.Context, accountID, mailboxID int64) (*Mailbox, error) {
	return scanMailbox(s.db.QueryRow(ctx, `
SELECT `+mailboxColumns+`
FROM mail_mailboxes WHERE account_id = ? AND id = ?`, accountID, mailboxID))
}

// DeleteMessage removes one message row (the M4 live delete/move ops land
// this after the server-side EXPUNGE; on a move the next sync rediscovers
// the copy in the destination mailbox). A missing row is ErrNotFound.
func (s *SQLStore) DeleteMessage(ctx context.Context, mailboxID, messageID int64) error {
	res, err := s.db.Exec(ctx, `
DELETE FROM mail_messages WHERE mailbox_id = ? AND id = ?`, mailboxID, messageID)
	if err != nil {
		return fmt.Errorf("mail: delete message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mail: delete message: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: message %d", ErrNotFound, messageID)
	}
	return nil
}

// SetMessagePreview fills one row's M6 list-preview columns — the sync-time
// fetcher is the only writer (rows insert with the empty defaults).
func (s *SQLStore) SetMessagePreview(ctx context.Context, mailboxID, uid int64, preview string, hasAttachments bool) error {
	if _, err := s.db.Exec(ctx, `
UPDATE mail_messages SET preview = ?, has_attachments = ? WHERE mailbox_id = ? AND uid = ?`,
		preview, hasAttachments, mailboxID, uid); err != nil {
		return fmt.Errorf("mail: set message preview: %w", err)
	}
	return nil
}

// searchMessagesMax bounds one unified-search query, mirroring the files
// SearchByName cap.
const searchMessagesMax = 20

// SearchMessages joins the caller's synced messages across every account
// and mailbox they own, matching subject and from_addr with the files
// SearchByName idiom: a case-insensitive contains (ILIKE on Postgres, LIKE
// elsewhere) with %/_ backslash-escaped, newest first. It backs the M6
// unified-search provider, so user isolation is the WHERE clause itself —
// another user's rows are never scanned.
func (s *SQLStore) SearchMessages(ctx context.Context, userID, term string, limit int) ([]MessageHit, error) {
	term = strings.TrimSpace(term)
	if userID == "" || term == "" {
		return nil, nil
	}
	if limit <= 0 || limit > searchMessagesMax {
		limit = searchMessagesMax
	}
	op := "LIKE"
	if s.db.Dialect() == database.DialectPostgres {
		op = "ILIKE"
	}
	q := fmt.Sprintf(`
SELECT msg.id, msg.uid, msg.mailbox_id, mb.account_id, msg.subject, msg.from_addr
FROM mail_messages msg
JOIN mail_mailboxes mb ON mb.id = msg.mailbox_id
JOIN mail_accounts a ON a.id = mb.account_id
WHERE a.user_id = ? AND (msg.subject %s ? ESCAPE '\' OR msg.from_addr %s ? ESCAPE '\')
ORDER BY msg.date_unix DESC, msg.id DESC
LIMIT ?`, op, op)
	rows, err := s.db.Query(ctx, q, userID, likeContains(term), likeContains(term), limit)
	if err != nil {
		return nil, fmt.Errorf("mail: search messages: %w", err)
	}
	defer rows.Close()
	out := make([]MessageHit, 0)
	for rows.Next() {
		var h MessageHit
		if err := rows.Scan(&h.ID, &h.UID, &h.MailboxID, &h.AccountID, &h.Subject, &h.FromAddr); err != nil {
			return nil, fmt.Errorf("mail: scan message hit: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail: search messages: %w", err)
	}
	return out, nil
}

// likeContains renders term as a LIKE/ILIKE contains pattern with the
// wildcard characters backslash-escaped — the files SearchByName helper,
// mirrored so the two search surfaces match identically.
func likeContains(term string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range term {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}

func scanMessage(row mailboxScanner) (*Message, error) {
	var m Message
	if err := row.Scan(&m.ID, &m.MailboxID, &m.UID, &m.MessageID, &m.Subject, &m.FromAddr,
		&m.ToAddrs, &m.DateUnix, &m.Flags, &m.Size, &m.Preview, &m.HasAttachments); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("mail: scan message: %w", err)
	}
	return &m, nil
}

// deleteAccountCascade removes one account and everything it owns in one
// transaction — the app-level cascade ADR-0108 §5 prescribes (migration
// 0028 declares no DB-level FK): messages first, then mailboxes, then the
// account row itself. The account statement carries the (userID, id) scope,
// so a missing or not-owned row rolls the whole cascade back as
// ErrNotFound.
func (s *SQLStore) deleteAccountCascade(ctx context.Context, userID string, id int64) (err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mail: delete account: begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rerr := tx.Rollback(); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	if _, err := tx.Exec(ctx, `
DELETE FROM mail_messages WHERE mailbox_id IN (SELECT id FROM mail_mailboxes WHERE account_id = ?)`, id); err != nil {
		return fmt.Errorf("mail: delete account: messages: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mail_mailboxes WHERE account_id = ?`, id); err != nil {
		return fmt.Errorf("mail: delete account: mailboxes: %w", err)
	}
	res, err := tx.Exec(ctx, `DELETE FROM mail_accounts WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return fmt.Errorf("mail: delete account: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mail: delete account: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mail: delete account: commit: %w", err)
	}
	committed = true
	return nil
}

type mailboxScanner interface {
	Scan(dest ...any) error
}

func scanMailbox(row mailboxScanner) (*Mailbox, error) {
	var m Mailbox
	if err := row.Scan(&m.ID, &m.AccountID, &m.Name, &m.Delimiter, &m.UIDValidity, &m.UIDNext,
		&m.LastSeenUID, &m.Selectable, &m.SpecialUse); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("mail: scan mailbox: %w", err)
	}
	return &m, nil
}

// placeholders renders n comma-joined question marks for an IN (...) list.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// int64Args boxes an id chunk for Exec.
func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
