package sharing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
)

// SQLShareStore is a ShareStore backed by database.DB.
type SQLShareStore struct {
	db database.DB
}

// NewSQLShareStore returns a ShareStore.
func NewSQLShareStore(db database.DB) *SQLShareStore {
	return &SQLShareStore{db: db}
}

func (s *SQLShareStore) Insert(ctx context.Context, sh *files.Share) error {
	if sh == nil || sh.OwnerUserID == 0 || sh.Token == "" {
		return fmt.Errorf("sharing: invalid share")
	}
	np, err := files.NormalizePath(sh.Path)
	if err != nil {
		return err
	}
	sh.Path = np
	if sh.ShareType == 0 {
		sh.ShareType = files.ShareTypeLink
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO shares (share_type, owner_user_id, file_path, item_type, token, password_hash, permissions, label, expire_ms, stime_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sh.ShareType, sh.OwnerUserID, sh.Path, sh.ItemType, sh.Token, sh.PasswordHash, sh.Permissions, sh.Label, sh.ExpireMs, sh.StimeMs)
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return files.ErrExists
		}
		return fmt.Errorf("sharing: insert: %w", err)
	}
	got, err := s.GetByToken(ctx, sh.Token)
	if err != nil {
		return err
	}
	*sh = *got
	return nil
}

func (s *SQLShareStore) GetByID(ctx context.Context, id int64) (*files.Share, error) {
	if id == 0 {
		return nil, files.ErrNotFound
	}
	row := s.db.QueryRow(ctx, `
SELECT id, share_type, owner_user_id, file_path, item_type, token, password_hash, permissions, label, expire_ms, stime_ms
FROM shares WHERE id = ?`, id)
	return scanShare(row)
}

func (s *SQLShareStore) GetByToken(ctx context.Context, token string) (*files.Share, error) {
	if token == "" {
		return nil, files.ErrNotFound
	}
	row := s.db.QueryRow(ctx, `
SELECT id, share_type, owner_user_id, file_path, item_type, token, password_hash, permissions, label, expire_ms, stime_ms
FROM shares WHERE token = ?`, token)
	return scanShare(row)
}

func (s *SQLShareStore) ListByOwner(ctx context.Context, ownerUserID int64, pathFilter string) ([]files.Share, error) {
	if ownerUserID == 0 {
		return nil, nil
	}
	var rows database.Rows
	var err error
	if pathFilter != "" {
		np, nerr := files.NormalizePath(pathFilter)
		if nerr != nil {
			return nil, nerr
		}
		rows, err = s.db.Query(ctx, `
SELECT id, share_type, owner_user_id, file_path, item_type, token, password_hash, permissions, label, expire_ms, stime_ms
FROM shares WHERE owner_user_id = ? AND file_path = ? ORDER BY id`, ownerUserID, np)
	} else {
		rows, err = s.db.Query(ctx, `
SELECT id, share_type, owner_user_id, file_path, item_type, token, password_hash, permissions, label, expire_ms, stime_ms
FROM shares WHERE owner_user_id = ? ORDER BY id`, ownerUserID)
	}
	if err != nil {
		return nil, fmt.Errorf("sharing: list: %w", err)
	}
	defer rows.Close()
	return scanShares(rows)
}

func (s *SQLShareStore) Update(ctx context.Context, sh *files.Share) error {
	if sh == nil || sh.ID == 0 {
		return fmt.Errorf("sharing: invalid share")
	}
	_, err := s.db.Exec(ctx, `
UPDATE shares SET password_hash = ?, permissions = ?, label = ?, expire_ms = ?
WHERE id = ?`, sh.PasswordHash, sh.Permissions, sh.Label, sh.ExpireMs, sh.ID)
	if err != nil {
		return fmt.Errorf("sharing: update: %w", err)
	}
	return nil
}

func (s *SQLShareStore) Delete(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `DELETE FROM shares WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sharing: delete: %w", err)
	}
	return nil
}

func (s *SQLShareStore) listByPrefix(ctx context.Context, ownerUserID int64, filePath string) ([]files.Share, error) {
	np, err := files.NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	like := np + "/%"
	if np == "/" {
		like = "/%"
	}
	rows, err := s.db.Query(ctx, `
SELECT id, share_type, owner_user_id, file_path, item_type, token, password_hash, permissions, label, expire_ms, stime_ms
FROM shares WHERE owner_user_id = ? AND (file_path = ? OR file_path LIKE ?) ORDER BY file_path`, ownerUserID, np, like)
	if err != nil {
		return nil, fmt.Errorf("sharing: list prefix: %w", err)
	}
	defer rows.Close()
	return scanShares(rows)
}

func (s *SQLShareStore) DeleteExpired(ctx context.Context, nowMs int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM shares WHERE expire_ms > 0 AND expire_ms <= ?`, nowMs)
	if err != nil {
		return fmt.Errorf("sharing: expire: %w", err)
	}
	return nil
}

func (s *SQLShareStore) DeleteByPath(ctx context.Context, ownerUserID int64, filePath string) error {
	np, err := files.NormalizePath(filePath)
	if err != nil {
		return err
	}
	like := np + "/%"
	if np == "/" {
		like = "/%"
	}
	_, err = s.db.Exec(ctx, `DELETE FROM shares WHERE owner_user_id = ? AND (file_path = ? OR file_path LIKE ?)`, ownerUserID, np, like)
	if err != nil {
		return fmt.Errorf("sharing: delete path: %w", err)
	}
	return nil
}

func (s *SQLShareStore) RenamePath(ctx context.Context, ownerUserID int64, srcPath, dstPath string) error {
	src, err := files.NormalizePath(srcPath)
	if err != nil {
		return err
	}
	dst, err := files.NormalizePath(dstPath)
	if err != nil {
		return err
	}
	items, err := s.listByPrefix(ctx, ownerUserID, src)
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
		_, err := s.db.Exec(ctx, `UPDATE shares SET file_path = ? WHERE id = ?`, next, items[i].ID)
		if err != nil {
			return fmt.Errorf("sharing: rename: %w", err)
		}
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanShares(rows database.Rows) ([]files.Share, error) {
	var out []files.Share
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sh)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sharing: list: %w", err)
	}
	return out, nil
}

func scanShare(row rowScanner) (*files.Share, error) {
	var sh files.Share
	err := row.Scan(&sh.ID, &sh.ShareType, &sh.OwnerUserID, &sh.Path, &sh.ItemType, &sh.Token, &sh.PasswordHash, &sh.Permissions, &sh.Label, &sh.ExpireMs, &sh.StimeMs)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, files.ErrNotFound
		}
		return nil, fmt.Errorf("sharing: scan: %w", err)
	}
	return &sh, nil
}
