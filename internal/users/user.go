package users

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("users: not found")
	ErrExists   = errors.New("users: already exists")
	ErrNoUsers  = errors.New("users: no users configured")
)

// User is a local account.
type User struct {
	ID           int64
	UID          string
	DisplayName  string
	Email        string
	PasswordHash string
	QuotaBytes   *int64
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Group is a local group.
type Group struct {
	ID          int64
	GID         string
	DisplayName string
}

// Store persists users.
type Store interface {
	Create(ctx context.Context, u *User) error
	GetByUID(ctx context.Context, uid string) (*User, error)
	GetByID(ctx context.Context, id int64) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	Search(ctx context.Context, term string, limit int) ([]User, error)
	SearchGroups(ctx context.Context, term string, limit int) ([]Group, error)
	UpdatePasswordHash(ctx context.Context, id int64, hash string) error
	Count(ctx context.Context) (int64, error)
	CreateGroup(ctx context.Context, g *Group) error
	GetGroupByGID(ctx context.Context, gid string) (*Group, error)
	AddGroupMember(ctx context.Context, gid, uid string) error
	UserGroupGIDs(ctx context.Context, uid string) ([]string, error)
}
