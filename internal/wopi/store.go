package wopi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// ErrTokenNotFound reports a missing or expired WOPI access token.
var ErrTokenNotFound = errors.New("wopi: token not found")

// Token is one WOPI access token (ADR-0106): a plaintext bearer token
// binding a user to one filecache id, with the write grant baked in at
// mint time.
type Token struct {
	Token     string
	UID       string
	FileID    int64
	CanWrite  bool
	ExpiresAt time.Time
}

// Store persists WOPI access tokens.
type Store interface {
	Insert(ctx context.Context, t *Token) error
	// GetByToken returns the token when it exists and has not expired at
	// now; missing and expired rows both yield ErrTokenNotFound.
	GetByToken(ctx context.Context, token string, now time.Time) (*Token, error)
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
}

// SQLStore is a Store backed by database.DB.
type SQLStore struct {
	db database.DB
}

// NewSQLStore returns a WOPI token Store.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) Insert(ctx context.Context, t *Token) error {
	if t == nil || t.Token == "" || t.UID == "" {
		return errors.New("wopi: invalid token")
	}
	canWrite := 0
	if t.CanWrite {
		canWrite = 1
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO wopi_tokens (token, uid, file_id, can_write, expires_at)
VALUES (?, ?, ?, ?, ?)`,
		t.Token, t.UID, t.FileID, canWrite, t.ExpiresAt.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("wopi: insert: %w", err)
	}
	return nil
}

func (s *SQLStore) GetByToken(ctx context.Context, token string, now time.Time) (*Token, error) {
	if token == "" {
		return nil, ErrTokenNotFound
	}
	var t Token
	var canWrite int
	var expires int64
	err := s.db.QueryRow(ctx, `
SELECT token, uid, file_id, can_write, expires_at FROM wopi_tokens
WHERE token = ? AND expires_at > ?`, token, now.UTC().UnixMilli()).
		Scan(&t.Token, &t.UID, &t.FileID, &canWrite, &expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("wopi: get: %w", err)
	}
	t.CanWrite = canWrite != 0
	t.ExpiresAt = time.UnixMilli(expires).UTC()
	return &t, nil
}

func (s *SQLStore) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM wopi_tokens WHERE expires_at <= ?`, now.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("wopi: delete expired: %w", err)
	}
	return res.RowsAffected()
}
