package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// SQLStore is a Store backed by database.DB.
type SQLStore struct {
	db    database.DB
	Clock func() time.Time
}

// NewSQLStore returns a mail account Store.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db, Clock: time.Now}
}

func (s *SQLStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

const accountColumns = `id, user_id, name, email, imap_host, imap_port, imap_ssl_mode, imap_user,
       smtp_host, smtp_port, smtp_ssl_mode, smtp_user, password_sealed, created_at, updated_at`

func (s *SQLStore) Create(ctx context.Context, a *Account) error {
	if a == nil || a.UserID == "" || a.Email == "" || a.IMAPHost == "" || a.IMAPUser == "" ||
		a.SMTPHost == "" || a.SMTPUser == "" || len(a.PasswordSealed) == 0 {
		return fmt.Errorf("%w: account", ErrInvalid)
	}
	if a.IMAPSSLMode == "" {
		a.IMAPSSLMode = SSLModeSSL
	}
	if a.SMTPSSLMode == "" {
		a.SMTPSSLMode = SSLModeSSL
	}
	now := s.now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	_, err := s.db.Exec(ctx, `
INSERT INTO mail_accounts (user_id, name, email, imap_host, imap_port, imap_ssl_mode, imap_user,
    smtp_host, smtp_port, smtp_ssl_mode, smtp_user, password_sealed, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.UserID, a.Name, a.Email, a.IMAPHost, a.IMAPPort, a.IMAPSSLMode, a.IMAPUser,
		a.SMTPHost, a.SMTPPort, a.SMTPSSLMode, a.SMTPUser, a.PasswordSealed,
		a.CreatedAt.Unix(), a.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("mail: create account: %w", err)
	}
	got, err := scanAccount(s.db.QueryRow(ctx, `
SELECT `+accountColumns+`
FROM mail_accounts WHERE user_id = ? ORDER BY id DESC LIMIT 1`, a.UserID))
	if err != nil {
		return err
	}
	*a = *got
	return nil
}

func (s *SQLStore) GetByID(ctx context.Context, userID string, id int64) (*Account, error) {
	return scanAccount(s.db.QueryRow(ctx, `
SELECT `+accountColumns+`
FROM mail_accounts WHERE user_id = ? AND id = ?`, userID, id))
}

func (s *SQLStore) ListByUser(ctx context.Context, userID string) ([]Account, error) {
	rows, err := s.db.Query(ctx, `
SELECT `+accountColumns+`
FROM mail_accounts WHERE user_id = ? ORDER BY id ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("mail: list accounts: %w", err)
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
		return nil, fmt.Errorf("mail: list accounts: %w", err)
	}
	return out, nil
}

// Update rewrites every mutable column of the row (userID, id) identifies;
// a missing or not-owned row is ErrNotFound. UpdatedAt moves to now.
func (s *SQLStore) Update(ctx context.Context, a *Account) error {
	if a == nil || a.ID <= 0 || a.UserID == "" {
		return fmt.Errorf("%w: account", ErrInvalid)
	}
	a.UpdatedAt = s.now()
	res, err := s.db.Exec(ctx, `
UPDATE mail_accounts SET name = ?, email = ?, imap_host = ?, imap_port = ?, imap_ssl_mode = ?, imap_user = ?,
    smtp_host = ?, smtp_port = ?, smtp_ssl_mode = ?, smtp_user = ?, password_sealed = ?, updated_at = ?
WHERE user_id = ? AND id = ?`,
		a.Name, a.Email, a.IMAPHost, a.IMAPPort, a.IMAPSSLMode, a.IMAPUser,
		a.SMTPHost, a.SMTPPort, a.SMTPSSLMode, a.SMTPUser, a.PasswordSealed, a.UpdatedAt.Unix(),
		a.UserID, a.ID)
	if err != nil {
		return fmt.Errorf("mail: update account: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mail: update account: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes the account and — app-level cascade, ADR-0108 §5 — every
// mailbox and message it owns, all in one transaction (see
// deleteAccountCascade). A missing or not-owned row is ErrNotFound and
// removes nothing.
func (s *SQLStore) Delete(ctx context.Context, userID string, id int64) error {
	return s.deleteAccountCascade(ctx, userID, id)
}

type accountScanner interface {
	Scan(dest ...any) error
}

func scanAccount(row accountScanner) (*Account, error) {
	var a Account
	var created, updated int64
	if err := row.Scan(&a.ID, &a.UserID, &a.Name, &a.Email, &a.IMAPHost, &a.IMAPPort, &a.IMAPSSLMode, &a.IMAPUser,
		&a.SMTPHost, &a.SMTPPort, &a.SMTPSSLMode, &a.SMTPUser, &a.PasswordSealed, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("mail: scan account: %w", err)
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	a.UpdatedAt = time.Unix(updated, 0).UTC()
	return &a, nil
}
