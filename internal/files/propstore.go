package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

const (
	// PropNSOwnCloud is the ownCloud WebDAV property namespace.
	PropNSOwnCloud = "http://owncloud.org/ns"
	// PropFavorite is the oc:favorite local name.
	PropFavorite = "favorite"
)

// FileProperty is one persisted WebDAV property.
type FileProperty struct {
	ID     int64
	UserID int64
	Path   string
	NS     string
	Name   string
	Value  string
}

// PropertyStore persists path-keyed WebDAV properties.
type PropertyStore interface {
	Get(ctx context.Context, userID int64, filePath, ns, name string) (*FileProperty, error)
	Set(ctx context.Context, p *FileProperty) error
	Remove(ctx context.Context, userID int64, filePath, ns, name string) error
	ListByPath(ctx context.Context, userID int64, filePath string) ([]FileProperty, error)
	DeleteByPath(ctx context.Context, userID int64, filePath string) error
	RenamePath(ctx context.Context, userID int64, srcPath, dstPath string) error
	CopyPath(ctx context.Context, userID int64, srcPath, dstPath string) error
}

// SQLPropertyStore is a PropertyStore backed by database.DB.
type SQLPropertyStore struct {
	db database.DB
}

// NewSQLPropertyStore returns a PropertyStore.
func NewSQLPropertyStore(db database.DB) *SQLPropertyStore {
	return &SQLPropertyStore{db: db}
}

func (s *SQLPropertyStore) Get(ctx context.Context, userID int64, filePath, ns, name string) (*FileProperty, error) {
	if userID == 0 || ns == "" || name == "" {
		return nil, ErrNotFound
	}
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRow(ctx, `
SELECT id, user_id, file_path, ns, name, value
FROM file_properties WHERE user_id = ? AND file_path = ? AND ns = ? AND name = ?`, userID, np, ns, name)
	p, err := scanFileProperty(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("files: property get: %w", err)
	}
	return p, nil
}

func (s *SQLPropertyStore) Set(ctx context.Context, p *FileProperty) error {
	if p == nil || p.UserID == 0 || p.NS == "" || p.Name == "" {
		return fmt.Errorf("files: invalid file property")
	}
	np, err := NormalizePath(p.Path)
	if err != nil {
		return err
	}
	p.Path = np
	existing, err := s.Get(ctx, p.UserID, p.Path, p.NS, p.Name)
	if err == nil {
		_, err = s.db.Exec(ctx, `UPDATE file_properties SET value = ? WHERE id = ?`, p.Value, existing.ID)
		if err != nil {
			return fmt.Errorf("files: property update: %w", err)
		}
		p.ID = existing.ID
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO file_properties (user_id, file_path, ns, name, value)
VALUES (?, ?, ?, ?, ?)`, p.UserID, p.Path, p.NS, p.Name, p.Value)
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("files: property insert: %w", err)
	}
	got, err := s.Get(ctx, p.UserID, p.Path, p.NS, p.Name)
	if err != nil {
		return err
	}
	*p = *got
	return nil
}

func (s *SQLPropertyStore) Remove(ctx context.Context, userID int64, filePath, ns, name string) error {
	np, err := NormalizePath(filePath)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM file_properties WHERE user_id = ? AND file_path = ? AND ns = ? AND name = ?`, userID, np, ns, name)
	if err != nil {
		return fmt.Errorf("files: property remove: %w", err)
	}
	return nil
}

func (s *SQLPropertyStore) ListByPath(ctx context.Context, userID int64, filePath string) ([]FileProperty, error) {
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, file_path, ns, name, value
FROM file_properties WHERE user_id = ? AND file_path = ? ORDER BY ns, name`, userID, np)
	if err != nil {
		return nil, fmt.Errorf("files: property list: %w", err)
	}
	defer rows.Close()
	return scanFileProperties(rows)
}

func (s *SQLPropertyStore) listByPrefix(ctx context.Context, userID int64, filePath string) ([]FileProperty, error) {
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	like := np + "/%"
	if np == "/" {
		like = "/%"
	}
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, file_path, ns, name, value
FROM file_properties WHERE user_id = ? AND (file_path = ? OR file_path LIKE ?) ORDER BY file_path, ns, name`, userID, np, like)
	if err != nil {
		return nil, fmt.Errorf("files: property list prefix: %w", err)
	}
	defer rows.Close()
	return scanFileProperties(rows)
}

func (s *SQLPropertyStore) DeleteByPath(ctx context.Context, userID int64, filePath string) error {
	np, err := NormalizePath(filePath)
	if err != nil {
		return err
	}
	like := np + "/%"
	if np == "/" {
		like = "/%"
	}
	_, err = s.db.Exec(ctx, `DELETE FROM file_properties WHERE user_id = ? AND (file_path = ? OR file_path LIKE ?)`, userID, np, like)
	if err != nil {
		return fmt.Errorf("files: property delete path: %w", err)
	}
	return nil
}

func (s *SQLPropertyStore) RenamePath(ctx context.Context, userID int64, srcPath, dstPath string) error {
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
		_, err := s.db.Exec(ctx, `UPDATE file_properties SET file_path = ? WHERE id = ?`, next, items[i].ID)
		if err != nil {
			return fmt.Errorf("files: property rename: %w", err)
		}
	}
	return nil
}

func (s *SQLPropertyStore) CopyPath(ctx context.Context, userID int64, srcPath, dstPath string) error {
	src, err := NormalizePath(srcPath)
	if err != nil {
		return err
	}
	dst, err := NormalizePath(dstPath)
	if err != nil {
		return err
	}
	items, err := s.ListByPath(ctx, userID, src)
	if err != nil {
		return err
	}
	for i := range items {
		cp := items[i]
		cp.ID = 0
		cp.Path = dst
		if err := s.Set(ctx, &cp); err != nil {
			return err
		}
	}
	return nil
}

func scanFileProperties(rows database.Rows) ([]FileProperty, error) {
	var out []FileProperty
	for rows.Next() {
		p, err := scanFileProperty(rows)
		if err != nil {
			return nil, fmt.Errorf("files: property scan: %w", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: property list: %w", err)
	}
	return out, nil
}

func scanFileProperty(row rowScanner) (*FileProperty, error) {
	var p FileProperty
	if err := row.Scan(&p.ID, &p.UserID, &p.Path, &p.NS, &p.Name, &p.Value); err != nil {
		return nil, err
	}
	return &p, nil
}
