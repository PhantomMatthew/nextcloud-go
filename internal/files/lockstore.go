package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// FileLock is one exclusive write lock on a files path.
type FileLock struct {
	ID        int64
	UserID    int64
	Path      string
	Token     string
	Owner     string
	TimeoutMs int64
	CreatedMs int64
}

// LockStore persists path-keyed exclusive write locks.
type LockStore interface {
	GetByPath(ctx context.Context, userID int64, filePath string) (*FileLock, error)
	GetByToken(ctx context.Context, token string) (*FileLock, error)
	Insert(ctx context.Context, l *FileLock) error
	UpdateTimeout(ctx context.Context, id, timeoutMs int64) error
	Delete(ctx context.Context, id int64) error
	DeleteByPath(ctx context.Context, userID int64, filePath string) error
	RenamePath(ctx context.Context, userID int64, srcPath, dstPath string) error
}

// SQLLockStore is a LockStore backed by database.DB.
type SQLLockStore struct {
	db database.DB
}

// NewSQLLockStore returns a LockStore.
func NewSQLLockStore(db database.DB) *SQLLockStore {
	return &SQLLockStore{db: db}
}

func (s *SQLLockStore) GetByPath(ctx context.Context, userID int64, filePath string) (*FileLock, error) {
	if userID == 0 {
		return nil, ErrNotFound
	}
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRow(ctx, `
SELECT id, user_id, file_path, token, owner, timeout_ms, created_ms
FROM file_locks WHERE user_id = ? AND file_path = ?`, userID, np)
	l, err := scanFileLock(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("files: lock get path: %w", err)
	}
	return l, nil
}

func (s *SQLLockStore) GetByToken(ctx context.Context, token string) (*FileLock, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(ctx, `
SELECT id, user_id, file_path, token, owner, timeout_ms, created_ms
FROM file_locks WHERE token = ?`, token)
	l, err := scanFileLock(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("files: lock get token: %w", err)
	}
	return l, nil
}

func (s *SQLLockStore) Insert(ctx context.Context, l *FileLock) error {
	if l == nil || l.UserID == 0 || l.Token == "" {
		return fmt.Errorf("files: invalid file lock")
	}
	np, err := NormalizePath(l.Path)
	if err != nil {
		return err
	}
	l.Path = np
	_, err = s.db.Exec(ctx, `
INSERT INTO file_locks (user_id, file_path, token, owner, timeout_ms, created_ms)
VALUES (?, ?, ?, ?, ?, ?)`, l.UserID, l.Path, l.Token, l.Owner, l.TimeoutMs, l.CreatedMs)
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("files: lock insert: %w", err)
	}
	got, err := s.GetByPath(ctx, l.UserID, l.Path)
	if err != nil {
		return err
	}
	*l = *got
	return nil
}

func (s *SQLLockStore) UpdateTimeout(ctx context.Context, id, timeoutMs int64) error {
	if id == 0 {
		return fmt.Errorf("files: invalid lock id")
	}
	_, err := s.db.Exec(ctx, `UPDATE file_locks SET timeout_ms = ? WHERE id = ?`, timeoutMs, id)
	if err != nil {
		return fmt.Errorf("files: lock update timeout: %w", err)
	}
	return nil
}

func (s *SQLLockStore) Delete(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `DELETE FROM file_locks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("files: lock delete: %w", err)
	}
	return nil
}

func (s *SQLLockStore) listByPrefix(ctx context.Context, userID int64, filePath string) ([]FileLock, error) {
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	like := np + "/%"
	if np == "/" {
		like = "/%"
	}
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, file_path, token, owner, timeout_ms, created_ms
FROM file_locks WHERE user_id = ? AND (file_path = ? OR file_path LIKE ?) ORDER BY file_path`, userID, np, like)
	if err != nil {
		return nil, fmt.Errorf("files: lock list prefix: %w", err)
	}
	defer rows.Close()
	var out []FileLock
	for rows.Next() {
		l, err := scanFileLock(rows)
		if err != nil {
			return nil, fmt.Errorf("files: lock scan: %w", err)
		}
		out = append(out, *l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: lock list: %w", err)
	}
	return out, nil
}

func (s *SQLLockStore) DeleteByPath(ctx context.Context, userID int64, filePath string) error {
	np, err := NormalizePath(filePath)
	if err != nil {
		return err
	}
	like := np + "/%"
	if np == "/" {
		like = "/%"
	}
	_, err = s.db.Exec(ctx, `DELETE FROM file_locks WHERE user_id = ? AND (file_path = ? OR file_path LIKE ?)`, userID, np, like)
	if err != nil {
		return fmt.Errorf("files: lock delete path: %w", err)
	}
	return nil
}

func (s *SQLLockStore) RenamePath(ctx context.Context, userID int64, srcPath, dstPath string) error {
	src, err := NormalizePath(srcPath)
	if err != nil {
		return err
	}
	dst, err := NormalizePath(dstPath)
	if err != nil {
		return err
	}
	items, err := s.listByPrefix(ctx, userID, src)
	if err != nil {
		return err
	}
	for i := range items {
		old := items[i].Path
		var next string
		if old == src {
			next = dst
		} else {
			next = dst + strings.TrimPrefix(old, src)
		}
		_, err := s.db.Exec(ctx, `UPDATE file_locks SET file_path = ? WHERE id = ?`, next, items[i].ID)
		if err != nil {
			return fmt.Errorf("files: lock rename: %w", err)
		}
	}
	return nil
}

func scanFileLock(row rowScanner) (*FileLock, error) {
	var l FileLock
	if err := row.Scan(&l.ID, &l.UserID, &l.Path, &l.Token, &l.Owner, &l.TimeoutMs, &l.CreatedMs); err != nil {
		return nil, err
	}
	return &l, nil
}
