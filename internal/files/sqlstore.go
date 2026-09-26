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

// Store persists filecache metadata.
type Store interface {
	EnsureRoot(ctx context.Context, userID int64) (*File, error)
	GetByPath(ctx context.Context, userID int64, p string) (*File, error)
	GetByID(ctx context.Context, id int64) (*File, error)
	ListChildren(ctx context.Context, userID, parentID int64) ([]File, error)
	Insert(ctx context.Context, f *File) error
	UpdateMeta(ctx context.Context, f *File) error
	UpdateMetaIfETag(ctx context.Context, f *File, expectETag string) error
	DeleteSubtree(ctx context.Context, userID int64, p string) error
	RenameSubtree(ctx context.Context, userID int64, srcPath, dstPath string, now time.Time) error
	Usage(ctx context.Context, userID int64) (int64, error)
	RecalcAncestors(ctx context.Context, userID int64, startParent *int64, now time.Time) error
	SearchByName(ctx context.Context, userID int64, term string, limit int) ([]File, error)
}

// SQLStore is a Store backed by database.DB.
type SQLStore struct {
	db database.DB
}

// NewSQLStore returns a filecache Store.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) EnsureRoot(ctx context.Context, userID int64) (*File, error) {
	f, err := s.GetByPath(ctx, userID, "/")
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	now := time.Now().UTC()
	root := &File{
		UserID:      userID,
		Name:        "",
		Path:        "/",
		IsDir:       true,
		Size:        0,
		Mtime:       now,
		MIME:        "httpd/unix-directory",
		Permissions: 31, // webdav.PermAll without importing webdav
	}
	root.ETag = ComputeDirETag(nil)
	if err := s.Insert(ctx, root); err != nil {
		if errors.Is(err, ErrExists) {
			return s.GetByPath(ctx, userID, "/")
		}
		return nil, err
	}
	return root, nil
}

func (s *SQLStore) GetByPath(ctx context.Context, userID int64, p string) (*File, error) {
	np, err := NormalizePath(p)
	if err != nil {
		return nil, err
	}
	return s.scanOne(s.db.QueryRow(ctx, `
SELECT id, user_id, parent_id, name, path, is_dir, size, mtime_ms, etag, checksum, mime, permissions, key_uuid
FROM files WHERE user_id = ? AND path = ?`, userID, np))
}

func (s *SQLStore) GetByID(ctx context.Context, id int64) (*File, error) {
	if id == 0 {
		return nil, ErrNotFound
	}
	return s.scanOne(s.db.QueryRow(ctx, `
SELECT id, user_id, parent_id, name, path, is_dir, size, mtime_ms, etag, checksum, mime, permissions, key_uuid
FROM files WHERE id = ?`, id))
}

func (s *SQLStore) getByID(ctx context.Context, id int64) (*File, error) {
	return s.GetByID(ctx, id)
}

func (s *SQLStore) ListChildren(ctx context.Context, userID, parentID int64) ([]File, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, parent_id, name, path, is_dir, size, mtime_ms, etag, checksum, mime, permissions, key_uuid
FROM files WHERE user_id = ? AND parent_id = ? ORDER BY path`, userID, parentID)
	if err != nil {
		return nil, fmt.Errorf("files: list: %w", err)
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: list: %w", err)
	}
	return out, nil
}

func (s *SQLStore) Insert(ctx context.Context, f *File) error {
	if f == nil || f.UserID == 0 {
		return fmt.Errorf("files: invalid file")
	}
	np, err := NormalizePath(f.Path)
	if err != nil {
		return err
	}
	f.Path = np
	if np == "/" {
		f.Name = ""
		f.ParentID = nil
		f.IsDir = true
	} else {
		if f.Name == "" {
			f.Name = path.Base(np)
		}
		parentPath := path.Dir(np)
		if parentPath == "." {
			parentPath = "/"
		}
		parent, err := s.GetByPath(ctx, f.UserID, parentPath)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrParentMissing
			}
			return err
		}
		if !parent.IsDir {
			return ErrNotDir
		}
		pid := parent.ID
		f.ParentID = &pid
	}
	if f.Mtime.IsZero() {
		f.Mtime = time.Now().UTC()
	}
	if f.MIME == "" {
		if f.IsDir {
			f.MIME = "httpd/unix-directory"
		} else {
			f.MIME = "application/octet-stream"
		}
	}
	isDir := 0
	if f.IsDir {
		isDir = 1
	}
	var parent any
	if f.ParentID != nil {
		parent = *f.ParentID
	}
	var checksum any
	if f.Checksum != "" {
		checksum = f.Checksum
	}
	var keyUUID any
	if len(f.KeyUUID) > 0 {
		keyUUID = f.KeyUUID
	}
	if f.ETag == "" {
		if f.IsDir {
			f.ETag = ComputeDirETag(nil)
		} else {
			f.ETag = "pending"
		}
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO files (user_id, parent_id, name, path, is_dir, size, mtime_ms, etag, checksum, mime, permissions, key_uuid)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.UserID, parent, f.Name, f.Path, isDir, f.Size, f.Mtime.UTC().UnixMilli(), f.ETag, checksum, f.MIME, f.Permissions, keyUUID)
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("files: insert: %w", err)
	}
	got, err := s.GetByPath(ctx, f.UserID, f.Path)
	if err != nil {
		return err
	}
	if !got.IsDir {
		got.ETag = ComputeFileETag(got.ID, got.Mtime, got.Size)
		if err := s.UpdateMeta(ctx, got); err != nil {
			return err
		}
	}
	*f = *got
	return nil
}

func (s *SQLStore) UpdateMeta(ctx context.Context, f *File) error {
	if f == nil || f.ID == 0 {
		return fmt.Errorf("files: invalid file")
	}
	query, args := updateMetaStmt(f)
	if _, err := s.db.Exec(ctx, query+`
WHERE id = ?`, append(args, f.ID)...); err != nil {
		return fmt.Errorf("files: update: %w", err)
	}
	return nil
}

// UpdateMetaIfETag is UpdateMeta guarded by the etag the caller read when it
// evaluated its write preconditions (ADR-0094): a zero-row update means a
// concurrent write landed first and reports ErrETagConflict.
func (s *SQLStore) UpdateMetaIfETag(ctx context.Context, f *File, expectETag string) error {
	if f == nil || f.ID == 0 {
		return fmt.Errorf("files: invalid file")
	}
	query, args := updateMetaStmt(f)
	res, err := s.db.Exec(ctx, query+`
WHERE id = ? AND etag = ?`, append(args, f.ID, expectETag)...)
	if err != nil {
		return fmt.Errorf("files: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("files: update: %w", err)
	}
	if n == 0 {
		return ErrETagConflict
	}
	return nil
}

// updateMetaStmt builds the shared UPDATE statement and column arguments for
// UpdateMeta and UpdateMetaIfETag; callers append their own WHERE clause and
// trailing arguments.
func updateMetaStmt(f *File) (string, []any) {
	isDir := 0
	if f.IsDir {
		isDir = 1
	}
	var checksum any
	if f.Checksum != "" {
		checksum = f.Checksum
	}
	var keyUUID any
	if len(f.KeyUUID) > 0 {
		keyUUID = f.KeyUUID
	}
	return `
UPDATE files SET name = ?, path = ?, is_dir = ?, size = ?, mtime_ms = ?, etag = ?, checksum = ?, mime = ?, permissions = ?, parent_id = ?, key_uuid = ?`,
		[]any{f.Name, f.Path, isDir, f.Size, f.Mtime.UTC().UnixMilli(), f.ETag, checksum, f.MIME, f.Permissions, nullInt(f.ParentID), keyUUID}
}

func (s *SQLStore) DeleteSubtree(ctx context.Context, userID int64, p string) error {
	np, err := NormalizePath(p)
	if err != nil {
		return err
	}
	if np == "/" {
		return ErrForbidden
	}
	f, err := s.GetByPath(ctx, userID, np)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM files WHERE id = ?`, f.ID)
	if err != nil {
		return fmt.Errorf("files: delete: %w", err)
	}
	return nil
}

func (s *SQLStore) RenameSubtree(ctx context.Context, userID int64, srcPath, dstPath string, now time.Time) error {
	src, err := NormalizePath(srcPath)
	if err != nil {
		return err
	}
	dst, err := NormalizePath(dstPath)
	if err != nil {
		return err
	}
	if src == "/" || dst == "/" {
		return ErrForbidden
	}
	node, err := s.GetByPath(ctx, userID, src)
	if err != nil {
		return err
	}
	if _, err := s.GetByPath(ctx, userID, dst); err == nil {
		return ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	dstParentPath := path.Dir(dst)
	if dstParentPath == "." {
		dstParentPath = "/"
	}
	dstParent, err := s.GetByPath(ctx, userID, dstParentPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrParentMissing
		}
		return err
	}
	if !dstParent.IsDir {
		return ErrNotDir
	}

	rows, err := s.db.Query(ctx, `
SELECT id, user_id, parent_id, name, path, is_dir, size, mtime_ms, etag, checksum, mime, permissions, key_uuid
FROM files WHERE user_id = ? AND (path = ? OR path LIKE ?)`, userID, src, src+"/%")
	if err != nil {
		return fmt.Errorf("files: rename list: %w", err)
	}
	defer rows.Close()
	var batch []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return err
		}
		batch = append(batch, *f)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("files: rename list: %w", err)
	}

	prefix := src + "/"
	newPrefix := dst + "/"
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for i := range batch {
		f := batch[i]
		switch {
		case f.Path == src:
			f.Path = dst
			f.Name = path.Base(dst)
			pid := dstParent.ID
			f.ParentID = &pid
		case strings.HasPrefix(f.Path, prefix):
			f.Path = newPrefix + strings.TrimPrefix(f.Path, prefix)
		default:
			continue
		}
		f.Mtime = now
		if !f.IsDir {
			f.ETag = ComputeFileETag(f.ID, f.Mtime, f.Size)
		}
		if err := s.UpdateMeta(ctx, &f); err != nil {
			return err
		}
	}
	_ = node
	return nil
}

const searchByNameMax = 20

func (s *SQLStore) SearchByName(ctx context.Context, userID int64, term string, limit int) ([]File, error) {
	term = strings.TrimSpace(term)
	if userID == 0 || term == "" {
		return nil, nil
	}
	if limit <= 0 || limit > searchByNameMax {
		limit = searchByNameMax
	}
	op := "LIKE"
	if s.db.Dialect() == database.DialectPostgres {
		op = "ILIKE"
	}
	q := fmt.Sprintf(`
SELECT id, user_id, parent_id, name, path, is_dir, size, mtime_ms, etag, checksum, mime, permissions, key_uuid
FROM files
WHERE user_id = ? AND path <> '/' AND name %s ? ESCAPE '\'
ORDER BY name, id
LIMIT ?`, op)
	rows, err := s.db.Query(ctx, q, userID, likeContains(term), limit)
	if err != nil {
		return nil, fmt.Errorf("files: search: %w", err)
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: search: %w", err)
	}
	return out, nil
}

func likeContains(term string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range term {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}

func (s *SQLStore) Usage(ctx context.Context, userID int64) (int64, error) {
	row := s.db.QueryRow(ctx, `SELECT COALESCE(SUM(size), 0) FROM files WHERE user_id = ? AND is_dir = 0`, userID)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("files: usage: %w", err)
	}
	return n, nil
}

func (s *SQLStore) RecalcAncestors(ctx context.Context, userID int64, startParent *int64, now time.Time) error {
	if startParent == nil {
		root, err := s.EnsureRoot(ctx, userID)
		if err != nil {
			return err
		}
		startParent = &root.ID
	}
	id := *startParent
	for {
		f, err := s.getByID(ctx, id)
		if err != nil {
			return err
		}
		if f.UserID != userID {
			return ErrForbidden
		}
		children, err := s.ListChildren(ctx, userID, f.ID)
		if err != nil {
			return err
		}
		var size int64
		for _, c := range children {
			size += c.Size
		}
		f.Size = size
		f.ETag = ComputeDirETag(children)
		if !now.IsZero() {
			f.Mtime = now.UTC()
		}
		if err := s.UpdateMeta(ctx, f); err != nil {
			return err
		}
		if f.ParentID == nil {
			return nil
		}
		id = *f.ParentID
	}
}

func (s *SQLStore) scanOne(row database.Row) (*File, error) {
	f, err := scanFile(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFile(row rowScanner) (*File, error) {
	var f File
	var parent sql.NullInt64
	var checksum sql.NullString
	var isDir int
	var mtimeMs int64
	if err := row.Scan(&f.ID, &f.UserID, &parent, &f.Name, &f.Path, &isDir, &f.Size, &mtimeMs, &f.ETag, &checksum, &f.MIME, &f.Permissions, &f.KeyUUID); err != nil {
		return nil, err
	}
	if parent.Valid {
		id := parent.Int64
		f.ParentID = &id
	}
	f.IsDir = isDir != 0
	f.Mtime = time.UnixMilli(mtimeMs).UTC()
	f.Checksum = checksum.String
	return &f, nil
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
