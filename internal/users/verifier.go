package users

import (
	"context"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

// PasswordVerifier authenticates against Store using a PasswordHasher.
type PasswordVerifier struct {
	store Store
	h     auth.PasswordHasher
}

// NewPasswordVerifier returns an auth.Verifier.
func NewPasswordVerifier(store Store, h auth.PasswordHasher) *PasswordVerifier {
	return &PasswordVerifier{store: store, h: h}
}

func (v *PasswordVerifier) Verify(ctx context.Context, user, password string) (*auth.Principal, error) {
	if user == "" || password == "" {
		return nil, auth.ErrNoCredentials
	}
	u, err := v.store.GetByUID(ctx, user)
	if err != nil {
		return nil, auth.ErrInvalidCredentials
	}
	if !u.Enabled {
		return nil, auth.ErrInvalidCredentials
	}
	ok, err := v.h.Verify(u.PasswordHash, password)
	if err != nil || !ok {
		return nil, auth.ErrInvalidCredentials
	}
	return &auth.Principal{
		UID:         u.UID,
		DisplayName: u.DisplayName,
		Enabled:     true,
		AuthMethod:  auth.AuthMethodBasic,
	}, nil
}
