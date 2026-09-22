package appconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// ErrNotFound is returned by Get when the (appid, key) row does not exist.
var ErrNotFound = errors.New("appconfig: not found")

// Store persists app-scoped configuration rows in the appconfig table.
type Store struct {
	db database.DB
}

// NewStore returns a Store backed by database.DB.
func NewStore(db database.DB) *Store {
	return &Store{db: db}
}

// Get returns the value stored under (appid, key), or ErrNotFound.
func (s *Store) Get(ctx context.Context, appid, key string) (string, error) {
	row := s.db.QueryRow(ctx, `
SELECT configvalue FROM appconfig WHERE appid = ? AND configkey = ?`, appid, key)
	var val string
	if err := row.Scan(&val); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s/%s", ErrNotFound, appid, key)
		}
		return "", fmt.Errorf("appconfig: get: %w", err)
	}
	return val, nil
}

// Set inserts or replaces the value under (appid, key).
func (s *Store) Set(ctx context.Context, appid, key, value string) error {
	var query string
	if s.db.Dialect() == database.DialectMySQL {
		query = `
INSERT INTO appconfig (appid, configkey, configvalue)
VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE
    configvalue = VALUES(configvalue)`
	} else {
		query = `
INSERT INTO appconfig (appid, configkey, configvalue)
VALUES (?, ?, ?)
ON CONFLICT (appid, configkey) DO UPDATE SET
    configvalue = excluded.configvalue`
	}
	if _, err := s.db.Exec(ctx, query, appid, key, value); err != nil {
		return fmt.Errorf("appconfig: set: %w", err)
	}
	return nil
}

// Delete removes the row under (appid, key); a missing row is not an error.
func (s *Store) Delete(ctx context.Context, appid, key string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM appconfig WHERE appid = ? AND configkey = ?`, appid, key); err != nil {
		return fmt.Errorf("appconfig: delete: %w", err)
	}
	return nil
}
