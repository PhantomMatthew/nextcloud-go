package auth

import "context"

const (
	AuthMethodBasic       = "basic"
	AuthMethodAppPassword = "app_password"
)

type Principal struct {
	UID         string
	DisplayName string
	Enabled     bool
	AuthMethod  string
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
