package contacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// SQLStore is a Store backed by database.DB.
type SQLStore struct {
	db    database.DB
	Clock func() time.Time
}

// NewSQLStore returns a contacts Store.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db, Clock: time.Now}
}

func (s *SQLStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *SQLStore) EnsureHome(ctx context.Context, userID int64) error {
	books, err := s.ListBooks(ctx, userID)
	if err != nil {
		return err
	}
	if len(books) > 0 {
		return nil
	}
	return s.CreateBook(ctx, &Addressbook{
		UserID:      userID,
		URI:         DefaultBookURI,
		DisplayName: DefaultDisplayName,
		Enabled:     true,
	})
}

func (s *SQLStore) ListBooks(ctx context.Context, userID int64) ([]Addressbook, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, uri, displayname, description, enabled, ctag, created_at, updated_at
FROM addressbooks WHERE user_id = ? ORDER BY uri`, userID)
	if err != nil {
		return nil, fmt.Errorf("contacts: list: %w", err)
	}
	defer rows.Close()
	var out []Addressbook
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (s *SQLStore) GetBookByURI(ctx context.Context, userID int64, uri string) (*Addressbook, error) {
	return scanBook(s.db.QueryRow(ctx, `
SELECT id, user_id, uri, displayname, description, enabled, ctag, created_at, updated_at
FROM addressbooks WHERE user_id = ? AND uri = ?`, userID, uri))
}

func (s *SQLStore) CreateBook(ctx context.Context, b *Addressbook) error {
	if b == nil || b.UserID == 0 || b.URI == "" {
		return fmt.Errorf("%w: addressbook", ErrInvalid)
	}
	if b.DisplayName == "" {
		b.DisplayName = b.URI
	}
	now := s.now()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	if b.CTag == 0 {
		b.CTag = 1
	}
	enabled := 0
	if b.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO addressbooks (user_id, uri, displayname, description, enabled, ctag, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.UserID, b.URI, b.DisplayName, b.Description, enabled, b.CTag,
		b.CreatedAt.UnixMilli(), b.UpdatedAt.UnixMilli())
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("contacts: insert: %w", err)
	}
	got, err := s.GetBookByURI(ctx, b.UserID, b.URI)
	if err != nil {
		return err
	}
	*b = *got
	return nil
}

func (s *SQLStore) UpdateBook(ctx context.Context, b *Addressbook) error {
	if b == nil || b.ID == 0 {
		return fmt.Errorf("%w: addressbook", ErrInvalid)
	}
	b.UpdatedAt = s.now()
	enabled := 0
	if b.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(ctx, `
UPDATE addressbooks SET displayname=?, description=?, enabled=?, updated_at=? WHERE id=?`,
		b.DisplayName, b.Description, enabled, b.UpdatedAt.UnixMilli(), b.ID)
	if err != nil {
		return fmt.Errorf("contacts: update: %w", err)
	}
	return nil
}

func (s *SQLStore) DeleteBook(ctx context.Context, userID int64, uri string) error {
	if _, err := s.GetBookByURI(ctx, userID, uri); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM addressbooks WHERE user_id = ? AND uri = ?`, userID, uri)
	if err != nil {
		return fmt.Errorf("contacts: delete: %w", err)
	}
	return nil
}

func (s *SQLStore) PutObject(ctx context.Context, userID int64, bookURI string, obj *Object) (bool, error) {
	if obj == nil {
		return false, fmt.Errorf("%w: object", ErrInvalid)
	}
	parsed, err := parseVCard(obj.Data)
	if err != nil {
		return false, err
	}
	book, err := s.GetBookByURI(ctx, userID, bookURI)
	if err != nil {
		return false, err
	}
	if existing, gerr := s.GetByUID(ctx, book.ID, parsed.UID); gerr == nil && existing.URI != obj.URI {
		return false, ErrConflict
	} else if gerr != nil && !errors.Is(gerr, ErrNotFound) {
		return false, gerr
	}
	now := s.now()
	obj.AddressbookID = book.ID
	obj.UID = parsed.UID
	obj.FN = parsed.FN
	obj.ETag = objectETag(obj.Data)
	obj.Size = int64(len(obj.Data))
	obj.UpdatedAt = now
	cur, err := s.GetObject(ctx, userID, bookURI, obj.URI)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if errors.Is(err, ErrNotFound) {
		obj.CreatedAt = now
		_, err = s.db.Exec(ctx, `
INSERT INTO addressbook_objects (addressbook_id, uri, uid, fn, etag, size, card_data, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			book.ID, obj.URI, obj.UID, obj.FN, obj.ETag, obj.Size, obj.Data,
			obj.CreatedAt.UnixMilli(), obj.UpdatedAt.UnixMilli())
		if err != nil {
			if database.IsUniqueViolation(s.db.Dialect(), err) {
				return false, ErrExists
			}
			return false, fmt.Errorf("contacts: insert object: %w", err)
		}
		if err := s.bumpCTag(ctx, book.ID, now); err != nil {
			return false, err
		}
		got, gerr := s.GetObject(ctx, userID, bookURI, obj.URI)
		if gerr != nil {
			return false, gerr
		}
		*obj = *got
		return true, nil
	}
	obj.ID = cur.ID
	obj.CreatedAt = cur.CreatedAt
	_, err = s.db.Exec(ctx, `
UPDATE addressbook_objects SET uid=?, fn=?, etag=?, size=?, card_data=?, updated_at=? WHERE id=?`,
		obj.UID, obj.FN, obj.ETag, obj.Size, obj.Data, obj.UpdatedAt.UnixMilli(), cur.ID)
	if err != nil {
		return false, fmt.Errorf("contacts: update object: %w", err)
	}
	if err := s.bumpCTag(ctx, book.ID, now); err != nil {
		return false, err
	}
	got, gerr := s.GetObject(ctx, userID, bookURI, obj.URI)
	if gerr != nil {
		return false, gerr
	}
	*obj = *got
	return false, nil
}

func (s *SQLStore) GetObject(ctx context.Context, userID int64, bookURI, uri string) (*Object, error) {
	book, err := s.GetBookByURI(ctx, userID, bookURI)
	if err != nil {
		return nil, err
	}
	return scanObject(s.db.QueryRow(ctx, `
SELECT id, addressbook_id, uri, uid, fn, etag, size, card_data, created_at, updated_at
FROM addressbook_objects WHERE addressbook_id = ? AND uri = ?`, book.ID, uri))
}

func (s *SQLStore) DeleteObject(ctx context.Context, userID int64, bookURI, uri string) error {
	book, err := s.GetBookByURI(ctx, userID, bookURI)
	if err != nil {
		return err
	}
	if _, err := s.GetObject(ctx, userID, bookURI, uri); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM addressbook_objects WHERE addressbook_id = ? AND uri = ?`, book.ID, uri)
	if err != nil {
		return fmt.Errorf("contacts: delete object: %w", err)
	}
	return s.bumpCTag(ctx, book.ID, s.now())
}

func (s *SQLStore) ListObjects(ctx context.Context, userID int64, bookURI string) ([]Object, error) {
	book, err := s.GetBookByURI(ctx, userID, bookURI)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
SELECT id, addressbook_id, uri, uid, fn, etag, size, card_data, created_at, updated_at
FROM addressbook_objects WHERE addressbook_id = ? ORDER BY uri`, book.ID)
	if err != nil {
		return nil, fmt.Errorf("contacts: objects: %w", err)
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

func (s *SQLStore) GetByUID(ctx context.Context, bookID int64, uid string) (*Object, error) {
	return scanObject(s.db.QueryRow(ctx, `
SELECT id, addressbook_id, uri, uid, fn, etag, size, card_data, created_at, updated_at
FROM addressbook_objects WHERE addressbook_id = ? AND uid = ?`, bookID, uid))
}

func (s *SQLStore) bumpCTag(ctx context.Context, bookID int64, now time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE addressbooks SET ctag = ctag + 1, updated_at = ? WHERE id = ?`, now.UnixMilli(), bookID)
	if err != nil {
		return fmt.Errorf("contacts: ctag: %w", err)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanBook(row scanner) (*Addressbook, error) {
	var b Addressbook
	var enabled int
	var created, updated int64
	if err := row.Scan(&b.ID, &b.UserID, &b.URI, &b.DisplayName, &b.Description, &enabled, &b.CTag, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("contacts: scan: %w", err)
	}
	b.Enabled = enabled != 0
	b.CreatedAt = time.UnixMilli(created).UTC()
	b.UpdatedAt = time.UnixMilli(updated).UTC()
	return &b, nil
}

func scanObject(row scanner) (*Object, error) {
	var o Object
	var created, updated int64
	if err := row.Scan(&o.ID, &o.AddressbookID, &o.URI, &o.UID, &o.FN, &o.ETag, &o.Size, &o.Data, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("contacts: scan object: %w", err)
	}
	o.CreatedAt = time.UnixMilli(created).UTC()
	o.UpdatedAt = time.UnixMilli(updated).UTC()
	return &o, nil
}

func (s *SQLStore) UpsertAddressbookShare(ctx context.Context, bookID, targetUserID int64, access string) error {
	if access != ShareAccessRead && access != ShareAccessReadWrite {
		return fmt.Errorf("%w: share access %q", ErrInvalid, access)
	}
	now := s.now().UnixMilli()
	res, err := s.db.Exec(ctx, `
UPDATE addressbook_shares SET access=?, updated_at=? WHERE addressbook_id=? AND target_user_id=?`,
		access, now, bookID, targetUserID)
	if err != nil {
		return fmt.Errorf("contacts: share update: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	if _, err := s.db.Exec(ctx, `
INSERT INTO addressbook_shares (addressbook_id, target_user_id, access, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)`, bookID, targetUserID, access, now, now); err != nil {
		return fmt.Errorf("contacts: share insert: %w", err)
	}
	return nil
}

func (s *SQLStore) DeleteAddressbookShare(ctx context.Context, bookID, targetUserID int64) error {
	res, err := s.db.Exec(ctx, `
DELETE FROM addressbook_shares WHERE addressbook_id = ? AND target_user_id = ?`, bookID, targetUserID)
	if err != nil {
		return fmt.Errorf("contacts: share delete: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) ListSharedAddressbooks(ctx context.Context, userID int64) ([]SharedAddressbook, error) {
	rows, err := s.db.Query(ctx, `
SELECT b.id, b.user_id, b.uri, b.displayname, b.description, b.enabled, b.ctag, b.created_at, b.updated_at,
       u.uid, s.access
FROM addressbook_shares s
JOIN addressbooks b ON b.id = s.addressbook_id
JOIN users u ON u.id = b.user_id
WHERE s.target_user_id = ? ORDER BY b.uri`, userID)
	if err != nil {
		return nil, fmt.Errorf("contacts: list shared: %w", err)
	}
	defer rows.Close()
	var out []SharedAddressbook
	for rows.Next() {
		sb, err := scanSharedBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sb)
	}
	return out, rows.Err()
}

func (s *SQLStore) GetSharedAddressbook(ctx context.Context, userID int64, uri string) (*SharedAddressbook, error) {
	row := s.db.QueryRow(ctx, `
SELECT b.id, b.user_id, b.uri, b.displayname, b.description, b.enabled, b.ctag, b.created_at, b.updated_at,
       u.uid, s.access
FROM addressbook_shares s
JOIN addressbooks b ON b.id = s.addressbook_id
JOIN users u ON u.id = b.user_id
WHERE s.target_user_id = ? AND b.uri = ?`, userID, uri)
	sb, err := scanSharedBook(row)
	if err != nil {
		return nil, err
	}
	return sb, nil
}

func scanSharedBook(row scanner) (*SharedAddressbook, error) {
	var sb SharedAddressbook
	var enabled int
	var created, updated int64
	err := row.Scan(&sb.ID, &sb.UserID, &sb.URI, &sb.DisplayName, &sb.Description, &enabled, &sb.CTag, &created, &updated, &sb.OwnerUID, &sb.Access)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("contacts: scan shared: %w", err)
	}
	sb.Enabled = enabled != 0
	sb.CreatedAt = time.UnixMilli(created).UTC()
	sb.UpdatedAt = time.UnixMilli(updated).UTC()
	return &sb, nil
}
