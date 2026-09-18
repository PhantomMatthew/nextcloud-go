package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const CookieName = "nc_session_id"

var (
	ErrNotFound = errors.New("session: not found")
	ErrExpired  = errors.New("session: expired")
)

// Session is a server-side login session.
type Session struct {
	ID         string
	UserID     int64
	UserAgent  string
	IP         string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// Store persists sessions.
type Store interface {
	Create(ctx context.Context, userID int64, ua, ip string, ttl time.Duration, now time.Time) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	Touch(ctx context.Context, id string, now time.Time, ttl time.Duration) error
	Delete(ctx context.Context, id string) error
}

// NewID returns 32 random bytes encoded as hex.
func NewID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
