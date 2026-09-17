package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// SQLStore persists app passwords.
type SQLStore struct {
	db database.DB
}

// NewSQLStore returns an app-password Store backed by db.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) Insert(ctx context.Context, t *Token) error {
	if t == nil || t.Hash == "" || t.ID == "" {
		return ErrTokenInvalid
	}
	q := `INSERT INTO app_passwords (id, user_id, token_hash, login_name, name, type, created_at)
SELECT ?, u.id, ?, ?, ?, ?, ? FROM users u WHERE u.uid = ?`
	_, err := s.db.Exec(ctx, q, t.ID, t.Hash, t.LoginName, t.Name, t.Type, t.CreatedAt.UTC().UnixMilli(), t.UID)
	if err != nil {
		return fmt.Errorf("auth: insert token: %w", err)
	}
	return nil
}

func (s *SQLStore) GetByHash(ctx context.Context, hash string) (*Token, error) {
	row := s.db.QueryRow(ctx, `
SELECT ap.id, ap.token_hash, u.uid, ap.login_name, ap.name, ap.type, ap.created_at
FROM app_passwords ap
JOIN users u ON u.id = ap.user_id
WHERE ap.token_hash = ?`, hash)
	var t Token
	var ms int64
	if err := row.Scan(&t.ID, &t.Hash, &t.UID, &t.LoginName, &t.Name, &t.Type, &ms); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("auth: get token: %w", err)
	}
	t.CreatedAt = time.UnixMilli(ms).UTC()
	return &t, nil
}

func (s *SQLStore) DeleteByHash(ctx context.Context, hash string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM app_passwords WHERE token_hash = ?`, hash)
	if err != nil {
		return fmt.Errorf("auth: delete token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTokenNotFound
	}
	return nil
}
