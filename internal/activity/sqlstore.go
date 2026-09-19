package activity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

// SQLStore is a Store backed by database.DB.
type SQLStore struct {
	db    database.DB
	Clock func() time.Time
}

// NewSQLStore returns an activity Store.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db, Clock: time.Now}
}

func (s *SQLStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *SQLStore) List(ctx context.Context, userID, since int64, limit int, sort string) ([]Event, error) {
	sort = strings.ToLower(strings.TrimSpace(sort))
	if sort == "" {
		sort = "desc"
	}
	if sort != "desc" && sort != "asc" {
		return nil, fmt.Errorf("%w: sort", ErrInvalid)
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	q := `
SELECT id, user_id, actor_uid, app, type, subject, subject_rich, subject_rich_parameters,
       message, object_type, object_id, object_name, link, icon, created_at
FROM activities WHERE user_id = ?`
	args := []any{userID}
	if since > 0 {
		if sort == "desc" {
			q += ` AND id < ?`
		} else {
			q += ` AND id > ?`
		}
		args = append(args, since)
	}
	if sort == "desc" {
		q += ` ORDER BY id DESC`
	} else {
		q += ` ORDER BY id ASC`
	}
	q += ` LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("activity: list: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (s *SQLStore) Insert(ctx context.Context, e *Event) error {
	if e == nil || e.UserID == 0 || e.App == "" {
		return fmt.Errorf("%w: event", ErrInvalid)
	}
	if e.SubjectRichParameters == "" {
		e.SubjectRichParameters = "{}"
	}
	now := s.now()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO activities (user_id, actor_uid, app, type, subject, subject_rich, subject_rich_parameters,
    message, object_type, object_id, object_name, link, icon, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.UserID, e.ActorUID, e.App, e.Type, e.Subject, e.SubjectRich, e.SubjectRichParameters,
		e.Message, e.ObjectType, e.ObjectID, e.ObjectName, e.Link, e.Icon, e.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("activity: insert: %w", err)
	}
	got, err := scanEvent(s.db.QueryRow(ctx, `
SELECT id, user_id, actor_uid, app, type, subject, subject_rich, subject_rich_parameters,
       message, object_type, object_id, object_name, link, icon, created_at
FROM activities WHERE user_id = ? ORDER BY id DESC LIMIT 1`, e.UserID))
	if err != nil {
		return err
	}
	*e = *got
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(row scanner) (*Event, error) {
	var e Event
	var created int64
	if err := row.Scan(&e.ID, &e.UserID, &e.ActorUID, &e.App, &e.Type, &e.Subject, &e.SubjectRich, &e.SubjectRichParameters,
		&e.Message, &e.ObjectType, &e.ObjectID, &e.ObjectName, &e.Link, &e.Icon, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("activity: scan: %w", err)
	}
	e.CreatedAt = time.UnixMilli(created).UTC()
	return &e, nil
}
