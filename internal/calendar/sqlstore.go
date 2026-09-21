package calendar

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

// NewSQLStore returns a calendar Store.
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
	cals, err := s.ListCalendars(ctx, userID)
	if err != nil {
		return err
	}
	if len(cals) > 0 {
		return nil
	}
	c := &Calendar{
		UserID:      userID,
		URI:         DefaultCalendarURI,
		DisplayName: DefaultDisplayName,
		Color:       DefaultCalendarColor,
		Enabled:     true,
	}
	return s.CreateCalendar(ctx, c)
}

func (s *SQLStore) ListCalendars(ctx context.Context, userID int64) ([]Calendar, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, user_id, uri, displayname, description, calendar_color, calendar_order, timezone, enabled, ctag, created_at, updated_at
FROM calendars WHERE user_id = ? ORDER BY uri`, userID)
	if err != nil {
		return nil, fmt.Errorf("calendar: list: %w", err)
	}
	defer rows.Close()
	var out []Calendar
	for rows.Next() {
		c, err := scanCalendar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *SQLStore) GetCalendarByURI(ctx context.Context, userID int64, uri string) (*Calendar, error) {
	return scanCalendar(s.db.QueryRow(ctx, `
SELECT id, user_id, uri, displayname, description, calendar_color, calendar_order, timezone, enabled, ctag, created_at, updated_at
FROM calendars WHERE user_id = ? AND uri = ?`, userID, uri))
}

func (s *SQLStore) CreateCalendar(ctx context.Context, c *Calendar) error {
	if c == nil || c.UserID == 0 || c.URI == "" {
		return fmt.Errorf("%w: calendar", ErrInvalid)
	}
	if c.DisplayName == "" {
		c.DisplayName = c.URI
	}
	if c.Color == "" {
		c.Color = DefaultCalendarColor
	}
	now := s.now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if c.CTag == 0 {
		c.CTag = 1
	}
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO calendars (user_id, uri, displayname, description, calendar_color, calendar_order, timezone, enabled, ctag, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.UserID, c.URI, c.DisplayName, c.Description, c.Color, c.Order, c.Timezone, enabled, c.CTag,
		c.CreatedAt.UnixMilli(), c.UpdatedAt.UnixMilli())
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("calendar: insert: %w", err)
	}
	got, err := s.GetCalendarByURI(ctx, c.UserID, c.URI)
	if err != nil {
		return err
	}
	*c = *got
	return nil
}

func (s *SQLStore) UpdateCalendar(ctx context.Context, c *Calendar) error {
	if c == nil || c.ID == 0 {
		return fmt.Errorf("%w: calendar", ErrInvalid)
	}
	c.UpdatedAt = s.now()
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(ctx, `
UPDATE calendars SET displayname=?, description=?, calendar_color=?, calendar_order=?, timezone=?, enabled=?, updated_at=?
WHERE id=?`,
		c.DisplayName, c.Description, c.Color, c.Order, c.Timezone, enabled, c.UpdatedAt.UnixMilli(), c.ID)
	if err != nil {
		return fmt.Errorf("calendar: update: %w", err)
	}
	return nil
}

func (s *SQLStore) DeleteCalendar(ctx context.Context, userID int64, uri string) error {
	_, err := s.GetCalendarByURI(ctx, userID, uri)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM calendars WHERE user_id = ? AND uri = ?`, userID, uri)
	if err != nil {
		return fmt.Errorf("calendar: delete: %w", err)
	}
	return nil
}

func (s *SQLStore) PutObject(ctx context.Context, userID int64, calendarURI string, obj *Object) (bool, error) {
	if obj == nil {
		return false, fmt.Errorf("%w: object", ErrInvalid)
	}
	parsed, err := parseICS(obj.Data)
	if err != nil {
		return false, err
	}
	cal, err := s.GetCalendarByURI(ctx, userID, calendarURI)
	if err != nil {
		return false, err
	}
	if existing, gerr := s.GetByUID(ctx, cal.ID, parsed.UID); gerr == nil && existing.URI != obj.URI {
		return false, ErrConflict
	} else if gerr != nil && !errors.Is(gerr, ErrNotFound) {
		return false, gerr
	}
	now := s.now()
	obj.CalendarID = cal.ID
	obj.UID = parsed.UID
	obj.Component = parsed.Component
	obj.FirstOccur = parsed.FirstOccur
	obj.LastOccur = parsed.LastOccur
	obj.ETag = objectETag(obj.Data)
	obj.Size = int64(len(obj.Data))
	obj.UpdatedAt = now
	cur, err := s.GetObject(ctx, userID, calendarURI, obj.URI)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if errors.Is(err, ErrNotFound) {
		obj.CreatedAt = now
		_, err = s.db.Exec(ctx, `
INSERT INTO calendar_objects (calendar_id, uri, uid, etag, size, component, first_occur_ms, last_occur_ms, calendar_data, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cal.ID, obj.URI, obj.UID, obj.ETag, obj.Size, obj.Component,
			obj.FirstOccur.UnixMilli(), obj.LastOccur.UnixMilli(), obj.Data,
			obj.CreatedAt.UnixMilli(), obj.UpdatedAt.UnixMilli())
		if err != nil {
			if database.IsUniqueViolation(s.db.Dialect(), err) {
				return false, ErrExists
			}
			return false, fmt.Errorf("calendar: insert object: %w", err)
		}
		if err := s.bumpCTag(ctx, cal.ID, now); err != nil {
			return false, err
		}
		got, gerr := s.GetObject(ctx, userID, calendarURI, obj.URI)
		if gerr != nil {
			return false, gerr
		}
		*obj = *got
		return true, nil
	}
	obj.ID = cur.ID
	obj.CreatedAt = cur.CreatedAt
	_, err = s.db.Exec(ctx, `
UPDATE calendar_objects SET uid=?, etag=?, size=?, component=?, first_occur_ms=?, last_occur_ms=?, calendar_data=?, updated_at=?
WHERE id=?`,
		obj.UID, obj.ETag, obj.Size, obj.Component, obj.FirstOccur.UnixMilli(), obj.LastOccur.UnixMilli(),
		obj.Data, obj.UpdatedAt.UnixMilli(), cur.ID)
	if err != nil {
		return false, fmt.Errorf("calendar: update object: %w", err)
	}
	if err := s.bumpCTag(ctx, cal.ID, now); err != nil {
		return false, err
	}
	got, gerr := s.GetObject(ctx, userID, calendarURI, obj.URI)
	if gerr != nil {
		return false, gerr
	}
	*obj = *got
	return false, nil
}

func (s *SQLStore) GetObject(ctx context.Context, userID int64, calendarURI, uri string) (*Object, error) {
	cal, err := s.GetCalendarByURI(ctx, userID, calendarURI)
	if err != nil {
		return nil, err
	}
	return scanObject(s.db.QueryRow(ctx, `
SELECT id, calendar_id, uri, uid, etag, size, component, first_occur_ms, last_occur_ms, calendar_data, created_at, updated_at
FROM calendar_objects WHERE calendar_id = ? AND uri = ?`, cal.ID, uri))
}

func (s *SQLStore) DeleteObject(ctx context.Context, userID int64, calendarURI, uri string) error {
	cal, err := s.GetCalendarByURI(ctx, userID, calendarURI)
	if err != nil {
		return err
	}
	if _, err := s.GetObject(ctx, userID, calendarURI, uri); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM calendar_objects WHERE calendar_id = ? AND uri = ?`, cal.ID, uri)
	if err != nil {
		return fmt.Errorf("calendar: delete object: %w", err)
	}
	return s.bumpCTag(ctx, cal.ID, s.now())
}

func (s *SQLStore) ListObjects(ctx context.Context, userID int64, calendarURI string) ([]Object, error) {
	cal, err := s.GetCalendarByURI(ctx, userID, calendarURI)
	if err != nil {
		return nil, err
	}
	return s.queryObjects(ctx, `
SELECT id, calendar_id, uri, uid, etag, size, component, first_occur_ms, last_occur_ms, calendar_data, created_at, updated_at
FROM calendar_objects WHERE calendar_id = ? ORDER BY uri`, cal.ID)
}

func (s *SQLStore) ObjectsInRange(ctx context.Context, userID int64, calendarURI string, start, end time.Time) ([]Object, error) {
	cal, err := s.GetCalendarByURI(ctx, userID, calendarURI)
	if err != nil {
		return nil, err
	}
	if start.IsZero() && end.IsZero() {
		return s.ListObjects(ctx, userID, calendarURI)
	}
	startMS := int64(0)
	endMS := int64(1 << 62)
	if !start.IsZero() {
		startMS = start.UnixMilli()
	}
	if !end.IsZero() {
		endMS = end.UnixMilli()
	}
	return s.queryObjects(ctx, `
SELECT id, calendar_id, uri, uid, etag, size, component, first_occur_ms, last_occur_ms, calendar_data, created_at, updated_at
FROM calendar_objects WHERE calendar_id = ? AND first_occur_ms < ? AND last_occur_ms >= ? ORDER BY uri`,
		cal.ID, endMS, startMS)
}

func (s *SQLStore) GetByUID(ctx context.Context, calendarID int64, uid string) (*Object, error) {
	return scanObject(s.db.QueryRow(ctx, `
SELECT id, calendar_id, uri, uid, etag, size, component, first_occur_ms, last_occur_ms, calendar_data, created_at, updated_at
FROM calendar_objects WHERE calendar_id = ? AND uid = ?`, calendarID, uid))
}

func (s *SQLStore) bumpCTag(ctx context.Context, calendarID int64, now time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE calendars SET ctag = ctag + 1, updated_at = ? WHERE id = ?`, now.UnixMilli(), calendarID)
	if err != nil {
		return fmt.Errorf("calendar: ctag: %w", err)
	}
	return nil
}

func (s *SQLStore) queryObjects(ctx context.Context, q string, args ...any) ([]Object, error) {
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("calendar: objects: %w", err)
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

type scanner interface {
	Scan(dest ...any) error
}

func scanCalendar(row scanner) (*Calendar, error) {
	var c Calendar
	var enabled int
	var created, updated int64
	if err := row.Scan(&c.ID, &c.UserID, &c.URI, &c.DisplayName, &c.Description, &c.Color, &c.Order, &c.Timezone, &enabled, &c.CTag, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("calendar: scan: %w", err)
	}
	c.Enabled = enabled != 0
	c.CreatedAt = time.UnixMilli(created).UTC()
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	return &c, nil
}

func scanObject(row scanner) (*Object, error) {
	var o Object
	var first, last, created, updated int64
	if err := row.Scan(&o.ID, &o.CalendarID, &o.URI, &o.UID, &o.ETag, &o.Size, &o.Component, &first, &last, &o.Data, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("calendar: scan object: %w", err)
	}
	o.FirstOccur = time.UnixMilli(first).UTC()
	o.LastOccur = time.UnixMilli(last).UTC()
	o.CreatedAt = time.UnixMilli(created).UTC()
	o.UpdatedAt = time.UnixMilli(updated).UTC()
	return &o, nil
}

func (s *SQLStore) UpsertCalendarShare(ctx context.Context, calendarID, targetUserID int64, access string) error {
	if access != ShareAccessRead && access != ShareAccessReadWrite {
		return fmt.Errorf("%w: share access %q", ErrInvalid, access)
	}
	now := s.now().UnixMilli()
	res, err := s.db.Exec(ctx, `
UPDATE calendar_shares SET access=?, updated_at=? WHERE calendar_id=? AND target_user_id=?`,
		access, now, calendarID, targetUserID)
	if err != nil {
		return fmt.Errorf("calendar: share update: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	if _, err := s.db.Exec(ctx, `
INSERT INTO calendar_shares (calendar_id, target_user_id, access, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)`, calendarID, targetUserID, access, now, now); err != nil {
		return fmt.Errorf("calendar: share insert: %w", err)
	}
	return nil
}

func (s *SQLStore) DeleteCalendarShare(ctx context.Context, calendarID, targetUserID int64) error {
	res, err := s.db.Exec(ctx, `
DELETE FROM calendar_shares WHERE calendar_id = ? AND target_user_id = ?`, calendarID, targetUserID)
	if err != nil {
		return fmt.Errorf("calendar: share delete: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) ListSharedCalendars(ctx context.Context, userID int64) ([]SharedCalendar, error) {
	rows, err := s.db.Query(ctx, `
SELECT c.id, c.user_id, c.uri, c.displayname, c.description, c.calendar_color, c.calendar_order, c.timezone, c.enabled, c.ctag, c.created_at, c.updated_at,
       u.uid, s.access
FROM calendar_shares s
JOIN calendars c ON c.id = s.calendar_id
JOIN users u ON u.id = c.user_id
WHERE s.target_user_id = ? ORDER BY c.uri`, userID)
	if err != nil {
		return nil, fmt.Errorf("calendar: list shared: %w", err)
	}
	defer rows.Close()
	var out []SharedCalendar
	for rows.Next() {
		sc, err := scanSharedCalendar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	return out, rows.Err()
}

func (s *SQLStore) GetSharedCalendar(ctx context.Context, userID int64, uri string) (*SharedCalendar, error) {
	row := s.db.QueryRow(ctx, `
SELECT c.id, c.user_id, c.uri, c.displayname, c.description, c.calendar_color, c.calendar_order, c.timezone, c.enabled, c.ctag, c.created_at, c.updated_at,
       u.uid, s.access
FROM calendar_shares s
JOIN calendars c ON c.id = s.calendar_id
JOIN users u ON u.id = c.user_id
WHERE s.target_user_id = ? AND c.uri = ?`, userID, uri)
	sc, err := scanSharedCalendar(row)
	if err != nil {
		return nil, err
	}
	return sc, nil
}

func scanSharedCalendar(row scanner) (*SharedCalendar, error) {
	var sc SharedCalendar
	var enabled int
	var created, updated int64
	err := row.Scan(&sc.ID, &sc.UserID, &sc.URI, &sc.DisplayName, &sc.Description, &sc.Color, &sc.Order, &sc.Timezone, &enabled, &sc.CTag, &created, &updated, &sc.OwnerUID, &sc.Access)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("calendar: scan shared: %w", err)
	}
	sc.Enabled = enabled != 0
	sc.CreatedAt = time.UnixMilli(created).UTC()
	sc.UpdatedAt = time.UnixMilli(updated).UTC()
	return &sc, nil
}
