package auth

import "context"

const (
	AuthMethodBasic       = "basic"
	AuthMethodAppPassword = "app_password"
	AuthMethodBearer      = "bearer"
	AuthMethodSession     = "session"
)

type Principal struct {
	UID         string
	DisplayName string
	Enabled     bool
	AuthMethod  string
	// UnlockedKey is the phase 5w-4 unlocked X25519 private key (ADR-0100
	// password-wrapped user keys), attached by session verification when the
	// session row carries a sealed key copy; nil for app-password, bearer,
	// and anonymous requests. It is key material: never log or serialize it,
	// and principals are never %v-dumped.
	UnlockedKey []byte
}

// UserInfo is the account projection auth needs without importing internal/users.
type UserInfo struct {
	ID          int64
	UID         string
	DisplayName string
	Enabled     bool
}

// UserSource looks up accounts for Bearer and session authentication.
type UserSource interface {
	GetByUID(ctx context.Context, uid string) (*UserInfo, error)
	GetByID(ctx context.Context, id int64) (*UserInfo, error)
}

type ctxKey struct{}

func WithUser(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func UserFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	if !ok || p == nil {
		return nil, false
	}
	return p, true
}
