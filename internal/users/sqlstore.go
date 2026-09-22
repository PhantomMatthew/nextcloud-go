package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// SQLStore persists users.
type SQLStore struct {
	db database.DB
}

// NewSQLStore returns a user Store backed by db.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) Create(ctx context.Context, u *User) error {
	if u == nil || u.UID == "" {
		return fmt.Errorf("users: invalid user")
	}
	now := time.Now().UTC().UnixMilli()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.UnixMilli(now).UTC()
	}
	u.UpdatedAt = u.CreatedAt
	var quota any
	if u.QuotaBytes != nil {
		quota = *u.QuotaBytes
	}
	enabled := 0
	if u.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO users (uid, display_name, email, password_hash, quota_bytes, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.UID, u.DisplayName, nullEmail(u.Email), u.PasswordHash, quota, enabled,
		u.CreatedAt.UnixMilli(), u.UpdatedAt.UnixMilli())
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("users: create: %w", err)
	}
	got, err := s.GetByUID(ctx, u.UID)
	if err != nil {
		return err
	}
	*u = *got
	return nil
}

func (s *SQLStore) GetByUID(ctx context.Context, uid string) (*User, error) {
	return s.getUser(ctx, `SELECT id, uid, display_name, email, password_hash, quota_bytes, enabled, created_at, updated_at
FROM users WHERE uid = ?`, uid)
}

func (s *SQLStore) GetByID(ctx context.Context, id int64) (*User, error) {
	return s.getUser(ctx, `SELECT id, uid, display_name, email, password_hash, quota_bytes, enabled, created_at, updated_at
FROM users WHERE id = ?`, id)
}

func (s *SQLStore) GetByEmail(ctx context.Context, email string) (*User, error) {
	return s.getUser(ctx, `SELECT id, uid, display_name, email, password_hash, quota_bytes, enabled, created_at, updated_at
FROM users WHERE email = ?`, email)
}

func (s *SQLStore) getUser(ctx context.Context, q string, arg any) (*User, error) {
	u, err := scanUserRow(s.db.QueryRow(ctx, q, arg))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("users: get: %w", err)
	}
	return u, nil
}

// SetEnabled flips the enabled flag for uid; unknown uids yield ErrNotFound.
func (s *SQLStore) SetEnabled(ctx context.Context, uid string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.Exec(ctx, `UPDATE users SET enabled = ?, updated_at = ? WHERE uid = ?`,
		v, time.Now().UTC().UnixMilli(), uid)
	if err != nil {
		return fmt.Errorf("users: set enabled: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes the user row and their group memberships. Files, shares, and
// other owned data are NOT cascaded; reassigning or purging them is the
// operator's responsibility (mirroring occ user:delete warnings).
func (s *SQLStore) Delete(ctx context.Context, uid string) error {
	u, err := s.GetByUID(ctx, uid)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM group_members WHERE user_id = ?`, u.ID); err != nil {
		return fmt.Errorf("users: delete memberships: %w", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		return fmt.Errorf("users: delete: %w", err)
	}
	return nil
}

const userColumns = `id, uid, display_name, email, password_hash, quota_bytes, enabled, created_at, updated_at`

func scanUserRow(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var email sql.NullString
	var quota sql.NullInt64
	var enabled int
	var created, updated int64
	if err := row.Scan(&u.ID, &u.UID, &u.DisplayName, &email, &u.PasswordHash, &quota, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	u.Email = email.String
	if quota.Valid {
		q := quota.Int64
		u.QuotaBytes = &q
	}
	u.Enabled = enabled != 0
	u.CreatedAt = time.UnixMilli(created).UTC()
	u.UpdatedAt = time.UnixMilli(updated).UTC()
	return &u, nil
}

func (s *SQLStore) scanUsers(rows database.Rows) ([]User, error) {
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// List returns users ordered by uid, including disabled accounts. A
// limit <= 0 returns all users; offset skips that many rows.
func (s *SQLStore) List(ctx context.Context, limit, offset int) ([]User, error) {
	q := `SELECT ` + userColumns + ` FROM users ORDER BY uid`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("users: list: %w", err)
	}
	return s.scanUsers(rows)
}

func (s *SQLStore) UpdatePasswordHash(ctx context.Context, id int64, hash string) error {
	res, err := s.db.Exec(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("users: update hash: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("users: count: %w", err)
	}
	return n, nil
}

func (s *SQLStore) CreateGroup(ctx context.Context, g *Group) error {
	if g == nil || g.GID == "" {
		return fmt.Errorf("users: invalid group")
	}
	display := g.DisplayName
	if display == "" {
		display = g.GID
	}
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.Exec(ctx, `INSERT INTO groups (gid, display_name, created_at) VALUES (?, ?, ?)`, g.GID, display, now)
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("users: create group: %w", err)
	}
	got, err := s.GetGroupByGID(ctx, g.GID)
	if err != nil {
		return err
	}
	*g = *got
	return nil
}

func (s *SQLStore) GetGroupByGID(ctx context.Context, gid string) (*Group, error) {
	if gid == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(ctx, `SELECT id, gid, display_name FROM groups WHERE gid = ?`, gid)
	var g Group
	if err := row.Scan(&g.ID, &g.GID, &g.DisplayName); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("users: get group: %w", err)
	}
	return &g, nil
}

func (s *SQLStore) AddGroupMember(ctx context.Context, gid, uid string) error {
	g, err := s.GetGroupByGID(ctx, gid)
	if err != nil {
		return err
	}
	u, err := s.GetByUID(ctx, uid)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO group_members (group_id, user_id) VALUES (?, ?)`, g.ID, u.ID)
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return nil
		}
		return fmt.Errorf("users: add member: %w", err)
	}
	return nil
}

func (s *SQLStore) UserGroupGIDs(ctx context.Context, uid string) ([]string, error) {
	u, err := s.GetByUID(ctx, uid)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
SELECT g.gid FROM groups g
INNER JOIN group_members m ON m.group_id = g.id
WHERE m.user_id = ? ORDER BY g.gid`, u.ID)
	if err != nil {
		return nil, fmt.Errorf("users: group gids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var gid string
		if err := rows.Scan(&gid); err != nil {
			return nil, err
		}
		out = append(out, gid)
	}
	return out, rows.Err()
}

// GroupMembers returns the uids of the members of gid, ordered by uid. A
// limit <= 0 returns all members. Unknown gids yield ErrNotFound.
func (s *SQLStore) GroupMembers(ctx context.Context, gid string, limit int) ([]string, error) {
	g, err := s.GetGroupByGID(ctx, gid)
	if err != nil {
		return nil, err
	}
	q := `
SELECT u.uid FROM users u
INNER JOIN group_members m ON m.user_id = u.id
WHERE m.group_id = ? ORDER BY u.uid`
	args := []any{g.ID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("users: group members: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}

// RemoveGroupMember drops uid from gid; unknown users or groups yield
// ErrNotFound, a missing membership is not an error.
func (s *SQLStore) RemoveGroupMember(ctx context.Context, gid, uid string) error {
	g, err := s.GetGroupByGID(ctx, gid)
	if err != nil {
		return err
	}
	u, err := s.GetByUID(ctx, uid)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM group_members WHERE group_id = ? AND user_id = ?`, g.ID, u.ID); err != nil {
		return fmt.Errorf("users: remove member: %w", err)
	}
	return nil
}

// DeleteGroup removes the group row and its memberships. Unknown gids yield
// ErrNotFound.
func (s *SQLStore) DeleteGroup(ctx context.Context, gid string) error {
	g, err := s.GetGroupByGID(ctx, gid)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM group_members WHERE group_id = ?`, g.ID); err != nil {
		return fmt.Errorf("users: delete group memberships: %w", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM groups WHERE id = ?`, g.ID); err != nil {
		return fmt.Errorf("users: delete group: %w", err)
	}
	return nil
}

// ListGroups returns groups ordered by gid. A limit <= 0 returns all groups;
// offset skips that many rows.
func (s *SQLStore) ListGroups(ctx context.Context, limit, offset int) ([]Group, error) {
	q := `SELECT id, gid, display_name FROM groups ORDER BY gid`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("users: list groups: %w", err)
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.GID, &g.DisplayName); err != nil {
			return nil, fmt.Errorf("users: list groups scan: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func nullEmail(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const searchMax = 20

// Search returns enabled users whose uid or display name contains term,
// ordered by uid. Limit is capped at 20.
func (s *SQLStore) Search(ctx context.Context, term string, limit int) ([]User, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return nil, nil
	}
	if limit <= 0 || limit > searchMax {
		limit = searchMax
	}
	op := "LIKE"
	if s.db.Dialect() == database.DialectPostgres {
		op = "ILIKE"
	}
	like := "%" + term + "%"
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
SELECT %s FROM users WHERE enabled = 1 AND (uid %s ? OR display_name %s ?) ORDER BY uid LIMIT ?`, userColumns, op, op), like, like, limit)
	if err != nil {
		return nil, fmt.Errorf("users: search: %w", err)
	}
	out, err := s.scanUsers(rows)
	if err != nil {
		return nil, fmt.Errorf("users: search scan: %w", err)
	}
	return out, nil
}

// SearchGroups returns groups whose gid or display name contains term,
// ordered by gid. Limit is capped at 20.
func (s *SQLStore) SearchGroups(ctx context.Context, term string, limit int) ([]Group, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return nil, nil
	}
	if limit <= 0 || limit > searchMax {
		limit = searchMax
	}
	op := "LIKE"
	if s.db.Dialect() == database.DialectPostgres {
		op = "ILIKE"
	}
	like := "%" + term + "%"
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
SELECT id, gid, display_name FROM groups WHERE gid %s ? OR display_name %s ? ORDER BY gid LIMIT ?`, op, op), like, like, limit)
	if err != nil {
		return nil, fmt.Errorf("users: search groups: %w", err)
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.GID, &g.DisplayName); err != nil {
			return nil, fmt.Errorf("users: search groups scan: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
