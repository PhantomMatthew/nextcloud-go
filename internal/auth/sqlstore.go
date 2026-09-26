package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// TokenKeysHook receives app-password revocations after the token row is
// gone (ADR-0102): the implementation deletes the token's app_token_keys
// wrap (cryptographic revocation). *encrypt.SQLResolver satisfies it
// structurally; auth must not import encrypt, so the seam is declared here.
type TokenKeysHook interface {
	OnTokenDeleted(ctx context.Context, appPasswordID string) error
}

// SQLStore persists app passwords.
type SQLStore struct {
	db database.DB
	// TokenKeys, when non-nil, is fired by DeleteByHash after the token row
	// is deleted. Best-effort by contract: a hook error NEVER fails the
	// delete — the token is already gone, and encryption reconcile's orphan
	// purge (app_token_keys rows whose app password no longer exists) is the
	// safety net for any wrap a failed hook leaves behind.
	TokenKeys TokenKeysHook
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
	// Resolve the id first so the token-keys hook (ADR-0102) can name the
	// wrap row after the token row is gone.
	var id string
	err := s.db.QueryRow(ctx, `SELECT id FROM app_passwords WHERE token_hash = ?`, hash).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return ErrTokenNotFound
		}
		return fmt.Errorf("auth: delete token: %w", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM app_passwords WHERE token_hash = ?`, hash); err != nil {
		return fmt.Errorf("auth: delete token: %w", err)
	}
	if s.TokenKeys != nil {
		//nolint:errcheck // best-effort by contract (see the field doc): a hook failure never fails the delete — reconcile's orphan purge reaps the leftover wrap
		_ = s.TokenKeys.OnTokenDeleted(ctx, id)
	}
	return nil
}
