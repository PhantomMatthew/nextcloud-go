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

// Store persists users.
type Store interface {
	Create(ctx context.Context, u *User) error
	GetByUID(ctx context.Context, uid string) (*User, error)
	UpdatePasswordHash(ctx context.Context, id int64, hash string) error
	Count(ctx context.Context) (int64, error)
}
