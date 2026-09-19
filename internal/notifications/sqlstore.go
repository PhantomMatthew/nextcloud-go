package notifications

import (
	"context"
	"crypto/sha1" //nolint:gosec // non-cryptographic: list ETag fingerprint
	"database/sql"
	"encoding/hex"
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

// NewSQLStore returns a notifications Store.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db, Clock: time.Now}
}

func (s *SQLStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *SQLStore) List(ctx context.Context, userID int64) ([]Notification, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, app, user_uid, object_type, object_id, subject, subject_rich, subject_rich_parameters,
       message, message_rich, message_rich_parameters, link, icon, should_notify, created_at
FROM notifications WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("notifications: list: %w", err)
	}
	defer rows.Close()
	out := make([]Notification, 0)
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (s *SQLStore) Get(ctx context.Context, userID, id int64) (*Notification, error) {
	return scanNotification(s.db.QueryRow(ctx, `
SELECT id, user_id, app, user_uid, object_type, object_id, subject, subject_rich, subject_rich_parameters,
       message, message_rich, message_rich_parameters, link, icon, should_notify, created_at
FROM notifications WHERE user_id = ? AND id = ?`, userID, id))
}

func (s *SQLStore) Delete(ctx context.Context, userID, id int64) error {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM notifications WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return fmt.Errorf("notifications: delete: %w", err)
	}
	return nil
}

func (s *SQLStore) DeleteAll(ctx context.Context, userID int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM notifications WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("notifications: delete all: %w", err)
	}
	return nil
}

func (s *SQLStore) Insert(ctx context.Context, n *Notification) error {
	if n == nil || n.UserID == 0 || n.App == "" || n.UserUID == "" {
		return fmt.Errorf("%w: notification", ErrInvalid)
	}
	if n.SubjectRichParameters == "" {
		n.SubjectRichParameters = "{}"
	}
	if n.MessageRichParameters == "" {
		n.MessageRichParameters = "[]"
	}
	now := s.now()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}
	should := 0
	if n.ShouldNotify {
		should = 1
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO notifications (user_id, app, user_uid, object_type, object_id, subject, subject_rich, subject_rich_parameters,
    message, message_rich, message_rich_parameters, link, icon, should_notify, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.UserID, n.App, n.UserUID, n.ObjectType, n.ObjectID, n.Subject, n.SubjectRich, n.SubjectRichParameters,
		n.Message, n.MessageRich, n.MessageRichParameters, n.Link, n.Icon, should, n.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("notifications: insert: %w", err)
	}
	got, err := scanNotification(s.db.QueryRow(ctx, `
SELECT id, user_id, app, user_uid, object_type, object_id, subject, subject_rich, subject_rich_parameters,
       message, message_rich, message_rich_parameters, link, icon, should_notify, created_at
FROM notifications WHERE user_id = ? ORDER BY id DESC LIMIT 1`, n.UserID))
	if err != nil {
		return err
	}
	*n = *got
	return nil
}

func (s *SQLStore) ListETag(ctx context.Context, userID int64) (string, error) {
	items, err := s.List(ctx, userID)
	if err != nil {
		return "", err
	}
	h := sha1.New() //nolint:gosec // non-cryptographic: list ETag fingerprint
	for i := range items {
		fmt.Fprintf(h, "%d|%d\n", items[i].ID, items[i].CreatedAt.UnixMilli())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanNotification(row scanner) (*Notification, error) {
	var n Notification
	var should int
	var created int64
	if err := row.Scan(&n.ID, &n.UserID, &n.App, &n.UserUID, &n.ObjectType, &n.ObjectID, &n.Subject, &n.SubjectRich, &n.SubjectRichParameters,
		&n.Message, &n.MessageRich, &n.MessageRichParameters, &n.Link, &n.Icon, &should, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("notifications: scan: %w", err)
	}
	n.ShouldNotify = should != 0
	n.CreatedAt = time.UnixMilli(created).UTC()
	return &n, nil
}
