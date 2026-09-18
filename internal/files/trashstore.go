package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// TrashItem is one row in trash_items.
type TrashItem struct {
	ID           int64
	UserID       int64
	OriginalPath string
	LocationID   string
	Name         string
	IsDir        bool
	Size         int64
	Deleted      time.Time
	DeletedBy    string
}

// TrashStore persists trashbin metadata.
type TrashStore interface {
	Insert(ctx context.Context, item *TrashItem) error
	GetByLocation(ctx context.Context, userID int64, locationID string) (*TrashItem, error)
	List(ctx context.Context, userID int64) ([]TrashItem, error)
	Delete(ctx context.Context, userID int64, locationID string) error
	DeleteExpired(ctx context.Context, userID int64, before time.Time) (int64, error)
}

// SQLTrashStore is a TrashStore backed by database.DB.
type SQLTrashStore struct {
	db database.DB
}

// NewSQLTrashStore returns a TrashStore.
func NewSQLTrashStore(db database.DB) *SQLTrashStore {
	return &SQLTrashStore{db: db}
}

func (s *SQLTrashStore) Insert(ctx context.Context, item *TrashItem) error {
	if item == nil || item.UserID == 0 || item.LocationID == "" {
		return fmt.Errorf("files: invalid trash item")
	}
	if !ValidLocationID(item.LocationID) {
		return ErrInvalidPath
	}
	if item.Name == "" {
		item.Name = path.Base(item.OriginalPath)
	}
	if item.Deleted.IsZero() {
		item.Deleted = time.Now().UTC()
	}
	isDir := 0
	if item.IsDir {
		isDir = 1
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO trash_items (user_id, original_path, location_id, name, is_dir, size, deleted_ms, deleted_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		item.UserID, item.OriginalPath, item.LocationID, item.Name, isDir, item.Size, item.Deleted.UTC().UnixMilli(), item.DeletedBy)
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("files: trash insert: %w", err)
	}
	got, err := s.GetByLocation(ctx, item.UserID, item.LocationID)
	if err != nil {
		return err
	}
	*item = *got
	return nil
}

func (s *SQLTrashStore) GetByLocation(ctx context.Context, userID int64, locationID string) (*TrashItem, error) {
	if userID == 0 || locationID == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(ctx, `
SELECT id, user_id, original_path, location_id, name, is_dir, size, deleted_ms, deleted_by
FROM trash_items WHERE user_id = ? AND location_id = ?`, userID, locationID)
	item, err := scanTrashItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("files: trash get: %w", err)
	}
	return item, nil
}

func (s *SQLTrashStore) List(ctx context.Context, userID int64) ([]TrashItem, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, original_path, location_id, name, is_dir, size, deleted_ms, deleted_by
FROM trash_items WHERE user_id = ? ORDER BY location_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("files: trash list: %w", err)
	}
	defer rows.Close()
	var out []TrashItem
	for rows.Next() {
		item, err := scanTrashItem(rows)
		if err != nil {
			return nil, fmt.Errorf("files: trash list scan: %w", err)
		}
		out = append(out, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: trash list: %w", err)
	}
	return out, nil
}

func (s *SQLTrashStore) Delete(ctx context.Context, userID int64, locationID string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM trash_items WHERE user_id = ? AND location_id = ?`, userID, locationID)
	if err != nil {
		return fmt.Errorf("files: trash delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("files: trash delete rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLTrashStore) DeleteExpired(ctx context.Context, userID int64, before time.Time) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM trash_items WHERE user_id = ? AND deleted_ms <= ?`, userID, before.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("files: trash expire: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("files: trash expire rows: %w", err)
	}
	return n, nil
}

func scanTrashItem(row rowScanner) (*TrashItem, error) {
	var item TrashItem
	var isDir int
	var deletedMs int64
	if err := row.Scan(&item.ID, &item.UserID, &item.OriginalPath, &item.LocationID, &item.Name, &isDir, &item.Size, &deletedMs, &item.DeletedBy); err != nil {
		return nil, err
	}
	item.IsDir = isDir != 0
	item.Deleted = time.UnixMilli(deletedMs).UTC()
	return &item, nil
}

// ValidLocationID reports whether id is a safe trash location name.
func ValidLocationID(id string) bool {
	if id == "" || len(id) > 255 {
		return false
	}
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return false
	}
	i := strings.LastIndex(id, ".d")
	if i <= 0 || i+2 >= len(id) {
		return false
	}
	rest := id[i+2:]
	for j, r := range rest {
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' && j > 0 {
			continue
		}
		return false
	}
	return true
}
