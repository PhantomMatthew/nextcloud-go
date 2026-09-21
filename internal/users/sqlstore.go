package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	row := s.db.QueryRow(ctx, q, arg)
	var u User
	var email sql.NullString
	var quota sql.NullInt64
	var enabled int
	var created, updated int64
	if err := row.Scan(&u.ID, &u.UID, &u.DisplayName, &email, &u.PasswordHash, &quota, &enabled, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("users: get: %w", err)
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

func nullEmail(s string) any {
	if s == "" {
		return nil
	}
	return s
}
