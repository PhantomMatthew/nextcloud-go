package mail

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// SSL mode values for the imap_ssl_mode / smtp_ssl_mode columns, mirroring
// the official Mail app's enum.
const (
	SSLModeSSL      = "ssl"
	SSLModeStartTLS = "starttls"
	SSLModeNone     = "none"
)

// ValidSSLMode reports whether m is one of the three SSL mode enum values.
func ValidSSLMode(m string) bool {
	switch m {
	case SSLModeSSL, SSLModeStartTLS, SSLModeNone:
		return true
	}
	return false
}

// Account is one user's mail account row. PasswordSealed is always the
// sealed credential blob (ADR-0108 §3) — the store never sees plaintext.
type Account struct {
	ID             int64
	UserID         string
	Name           string
	Email          string
	IMAPHost       string
	IMAPPort       int
	IMAPSSLMode    string
	IMAPUser       string
	SMTPHost       string
	SMTPPort       int
	SMTPSSLMode    string
	SMTPUser       string
	PasswordSealed []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Store persists mail accounts. Every read/write is scoped to the owning
// user: a row another user owns is indistinguishable from a missing one.
type Store interface {
	Create(ctx context.Context, a *Account) error
	GetByID(ctx context.Context, userID string, id int64) (*Account, error)
	ListByUser(ctx context.Context, userID string) ([]Account, error)
	Update(ctx context.Context, a *Account) error
	Delete(ctx context.Context, userID string, id int64) error
}

// ErrNotFound reports a missing (or not-owned) account row.
var ErrNotFound = errors.New("mail: account not found")

// ErrInvalid reports an account row missing its required fields.
var ErrInvalid = errors.New("mail: invalid account")

// joinPasswords packs the IMAP/SMTP password pair into the single
// password_sealed plaintext: be16(len(imap)) || imap || smtp. M1's schema
// carries one sealed column (ADR-0108 §3), so both credentials ride in it.
func joinPasswords(imap, smtp string) ([]byte, error) {
	if len(imap) > 0xffff {
		return nil, errors.New("mail: password too long")
	}
	out := make([]byte, 2, 2+len(imap)+len(smtp))
	//nolint:gosec // G115: length is bounds-checked against 0xffff above
	binary.BigEndian.PutUint16(out, uint16(len(imap)))
	out = append(out, imap...)
	out = append(out, smtp...)
	return out, nil
}

func splitPasswords(packed []byte) (imap, smtp string, err error) {
	if len(packed) < 2 {
		return "", "", fmt.Errorf("%w: packed passwords truncated", ErrCredential)
	}
	n := int(binary.BigEndian.Uint16(packed))
	if len(packed) < 2+n {
		return "", "", fmt.Errorf("%w: packed passwords truncated", ErrCredential)
	}
	return string(packed[2 : 2+n]), string(packed[2+n:]), nil
}
