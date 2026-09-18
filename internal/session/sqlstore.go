package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// SQLStore persists sessions.
type SQLStore struct {
	db database.DB
}

// NewSQLStore returns a session Store backed by db.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) Create(ctx context.Context, userID int64, ua, ip string, ttl time.Duration, now time.Time) (*Session, error) {
	if userID == 0 {
		return nil, fmt.Errorf("session: invalid user")
	}
	now = now.UTC()
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	sess := &Session{
		ID:         id,
		UserID:     userID,
		UserAgent:  ua,
		IP:         ip,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(ttl),
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO sessions (id, user_id, user_agent, ip, created_at, last_seen_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID, sess.UserAgent, sess.IP,
		sess.CreatedAt.UnixMilli(), sess.LastSeenAt.UnixMilli(), sess.ExpiresAt.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("session: create: %w", err)
	}
	return sess, nil
}

func (s *SQLStore) Get(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(ctx, `
SELECT id, user_id, user_agent, ip, created_at, last_seen_at, expires_at
FROM sessions WHERE id = ?`, id)
	var sess Session
	var created, last, exp int64
	var ua, ip sql.NullString
	if err := row.Scan(&sess.ID, &sess.UserID, &ua, &ip, &created, &last, &exp); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("session: get: %w", err)
	}
	sess.UserAgent = ua.String
	sess.IP = ip.String
	sess.CreatedAt = time.UnixMilli(created).UTC()
	sess.LastSeenAt = time.UnixMilli(last).UTC()
	sess.ExpiresAt = time.UnixMilli(exp).UTC()
	if !sess.ExpiresAt.After(time.Now().UTC()) {
		return nil, ErrExpired
	}
	return &sess, nil
}

func (s *SQLStore) Touch(ctx context.Context, id string, now time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now = now.UTC()
	res, err := s.db.Exec(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		now.UnixMilli(), now.Add(ttl).UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("session: touch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
