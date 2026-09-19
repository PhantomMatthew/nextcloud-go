package ocm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
)

// SQLStore is a Store backed by database.DB.
type SQLStore struct {
	db    database.DB
	Clock func() time.Time
}

// NewSQLStore returns an OCM incoming Store.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db, Clock: time.Now}
}

func (s *SQLStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *SQLStore) List(ctx context.Context, userID int64) ([]Incoming, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, user_uid, name, remote, remote_id, owner, token, item_type, permissions, accepted, created_at
FROM ocm_incoming WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("ocm: list: %w", err)
	}
	defer rows.Close()
	out := make([]Incoming, 0)
	for rows.Next() {
		in, err := scanIncoming(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *in)
	}
	return out, rows.Err()
}

func (s *SQLStore) Get(ctx context.Context, userID, id int64) (*Incoming, error) {
	return scanIncoming(s.db.QueryRow(ctx, `
SELECT id, user_id, user_uid, name, remote, remote_id, owner, token, item_type, permissions, accepted, created_at
FROM ocm_incoming WHERE user_id = ? AND id = ?`, userID, id))
}

func (s *SQLStore) Delete(ctx context.Context, userID, id int64) error {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM ocm_incoming WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return fmt.Errorf("ocm: delete: %w", err)
	}
	return nil
}

func (s *SQLStore) Insert(ctx context.Context, in *Incoming) error {
	if in == nil || in.UserID == 0 || in.UserUID == "" || in.Name == "" || in.Remote == "" || in.RemoteID == "" {
		return fmt.Errorf("%w: incoming", ErrInvalid)
	}
	if in.ItemType == "" {
		in.ItemType = "file"
	}
	if in.Permissions == 0 {
		in.Permissions = 1
	}
	if in.Accepted == 0 {
		in.Accepted = 1
	}
	now := s.now()
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO ocm_incoming (user_id, user_uid, name, remote, remote_id, owner, token, item_type, permissions, accepted, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, in.UserUID, in.Name, in.Remote, in.RemoteID, in.Owner, in.Token, in.ItemType, in.Permissions, in.Accepted, in.CreatedAt.UnixMilli())
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrConflict
		}
		return fmt.Errorf("ocm: insert: %w", err)
	}
	got, err := scanIncoming(s.db.QueryRow(ctx, `
SELECT id, user_id, user_uid, name, remote, remote_id, owner, token, item_type, permissions, accepted, created_at
FROM ocm_incoming WHERE user_id = ? ORDER BY id DESC LIMIT 1`, in.UserID))
	if err != nil {
		return err
	}
	*in = *got
	return nil
}

func (s *SQLStore) ListIncoming(ctx context.Context, shareeUID string) ([]files.IncomingMount, error) {
	if s == nil || shareeUID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, user_uid, name, remote, remote_id, owner, token, item_type, permissions, accepted, created_at
FROM ocm_incoming WHERE user_uid = ? AND accepted != 0 ORDER BY id DESC`, shareeUID)
	if err != nil {
		return nil, fmt.Errorf("ocm: list incoming: %w", err)
	}
	defer rows.Close()
	out := make([]files.IncomingMount, 0)
	for rows.Next() {
		in, err := scanIncoming(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, files.IncomingMount{
			Mount:       "/" + in.Name,
			Permissions: in.Permissions,
			ItemType:    in.ItemType,
			Remote:      true,
		})
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanIncoming(row scanner) (*Incoming, error) {
	var in Incoming
	var created int64
	if err := row.Scan(&in.ID, &in.UserID, &in.UserUID, &in.Name, &in.Remote, &in.RemoteID, &in.Owner, &in.Token, &in.ItemType, &in.Permissions, &in.Accepted, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("ocm: scan: %w", err)
	}
	in.CreatedAt = time.UnixMilli(created).UTC()
	return &in, nil
}
