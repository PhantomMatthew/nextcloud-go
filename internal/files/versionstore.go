package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// FileVersion is one historical snapshot of a file.
type FileVersion struct {
	ID       int64
	UserID   int64
	Path     string
	Revision string
	Size     int64
	Checksum string
	Created  time.Time
}

// VersionStore persists file version metadata.
type VersionStore interface {
	Insert(ctx context.Context, v *FileVersion) error
	Get(ctx context.Context, userID int64, filePath, revision string) (*FileVersion, error)
	ListByPath(ctx context.Context, userID int64, filePath string) ([]FileVersion, error)
	Delete(ctx context.Context, userID int64, filePath, revision string) error
	DeleteByPath(ctx context.Context, userID int64, filePath string) ([]FileVersion, error)
	DeleteExpired(ctx context.Context, userID int64, filePath string, before time.Time) ([]FileVersion, error)
	RenamePath(ctx context.Context, userID int64, srcPath, dstPath string) error
}

// SQLVersionStore is a VersionStore backed by database.DB.
type SQLVersionStore struct {
	db database.DB
}

// NewSQLVersionStore returns a VersionStore.
func NewSQLVersionStore(db database.DB) *SQLVersionStore {
	return &SQLVersionStore{db: db}
}

func (s *SQLVersionStore) Insert(ctx context.Context, v *FileVersion) error {
	if v == nil || v.UserID == 0 || v.Path == "" || v.Revision == "" {
		return fmt.Errorf("files: invalid file version")
	}
	if !ValidRevision(v.Revision) {
		return ErrInvalidPath
	}
	np, err := NormalizePath(v.Path)
	if err != nil {
		return err
	}
	v.Path = np
	if v.Created.IsZero() {
		v.Created = time.Now().UTC()
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO file_versions (user_id, file_path, revision, size, checksum, created_ms)
VALUES (?, ?, ?, ?, ?, ?)`,
		v.UserID, v.Path, v.Revision, v.Size, v.Checksum, v.Created.UTC().UnixMilli())
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("files: version insert: %w", err)
	}
	got, err := s.Get(ctx, v.UserID, v.Path, v.Revision)
	if err != nil {
		return err
	}
	*v = *got
	return nil
}

func (s *SQLVersionStore) Get(ctx context.Context, userID int64, filePath, revision string) (*FileVersion, error) {
	if userID == 0 || filePath == "" || revision == "" {
		return nil, ErrNotFound
	}
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRow(ctx, `
SELECT id, user_id, file_path, revision, size, checksum, created_ms
FROM file_versions WHERE user_id = ? AND file_path = ? AND revision = ?`, userID, np, revision)
	v, err := scanFileVersion(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("files: version get: %w", err)
	}
	return v, nil
}

func (s *SQLVersionStore) ListByPath(ctx context.Context, userID int64, filePath string) ([]FileVersion, error) {
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, file_path, revision, size, checksum, created_ms
FROM file_versions WHERE user_id = ? AND file_path = ? ORDER BY revision`, userID, np)
	if err != nil {
		return nil, fmt.Errorf("files: version list: %w", err)
	}
	defer rows.Close()
	return scanFileVersions(rows)
}

func (s *SQLVersionStore) listByPrefix(ctx context.Context, userID int64, filePath string) ([]FileVersion, error) {
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	like := np + "/%"
	if np == "/" {
		like = "/%"
	}
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, file_path, revision, size, checksum, created_ms
FROM file_versions WHERE user_id = ? AND (file_path = ? OR file_path LIKE ?) ORDER BY file_path, revision`, userID, np, like)
	if err != nil {
		return nil, fmt.Errorf("files: version list prefix: %w", err)
	}
	defer rows.Close()
	return scanFileVersions(rows)
}

func (s *SQLVersionStore) Delete(ctx context.Context, userID int64, filePath, revision string) error {
	np, err := NormalizePath(filePath)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(ctx, `DELETE FROM file_versions WHERE user_id = ? AND file_path = ? AND revision = ?`, userID, np, revision)
	if err != nil {
		return fmt.Errorf("files: version delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("files: version delete rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLVersionStore) DeleteByPath(ctx context.Context, userID int64, filePath string) ([]FileVersion, error) {
	items, err := s.listByPrefix(ctx, userID, filePath)
	if err != nil {
		return nil, err
	}
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	like := np + "/%"
	if np == "/" {
		like = "/%"
	}
	_, err = s.db.Exec(ctx, `DELETE FROM file_versions WHERE user_id = ? AND (file_path = ? OR file_path LIKE ?)`, userID, np, like)
	if err != nil {
		return nil, fmt.Errorf("files: version delete path: %w", err)
	}
	return items, nil
}

func (s *SQLVersionStore) DeleteExpired(ctx context.Context, userID int64, filePath string, before time.Time) ([]FileVersion, error) {
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, file_path, revision, size, checksum, created_ms
FROM file_versions WHERE user_id = ? AND file_path = ? AND created_ms <= ?`, userID, np, before.UTC().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("files: version expire list: %w", err)
	}
	expired, err := scanFileVersions(rows)
	if closeErr := rows.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if len(expired) == 0 {
		return nil, nil
	}
	_, err = s.db.Exec(ctx, `DELETE FROM file_versions WHERE user_id = ? AND file_path = ? AND created_ms <= ?`, userID, np, before.UTC().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("files: version expire: %w", err)
	}
	return expired, nil
}

func (s *SQLVersionStore) RenamePath(ctx context.Context, userID int64, srcPath, dstPath string) error {
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
		_, err := s.db.Exec(ctx, `UPDATE file_versions SET file_path = ? WHERE id = ?`, next, items[i].ID)
		if err != nil {
			return fmt.Errorf("files: version rename: %w", err)
		}
	}
	return nil
}

func scanFileVersions(rows database.Rows) ([]FileVersion, error) {
	var out []FileVersion
	for rows.Next() {
		v, err := scanFileVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("files: version scan: %w", err)
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: version list: %w", err)
	}
	return out, nil
}

func scanFileVersion(row rowScanner) (*FileVersion, error) {
	var v FileVersion
	var createdMs int64
	if err := row.Scan(&v.ID, &v.UserID, &v.Path, &v.Revision, &v.Size, &v.Checksum, &createdMs); err != nil {
		return nil, err
	}
	v.Created = time.UnixMilli(createdMs).UTC()
	return &v, nil
}

// ValidRevision reports whether revision is a timestamp name with optional -n suffix.
func ValidRevision(rev string) bool {
	if rev == "" || len(rev) > 64 || strings.ContainsAny(rev, "/\\") || strings.Contains(rev, "..") {
		return false
	}
	for i, r := range rev {
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' && i > 0 {
			continue
		}
		return false
	}
	return rev[0] >= '0' && rev[0] <= '9'
}
