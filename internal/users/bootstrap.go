package users

import (
	"context"
	"log/slog"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

// BootstrapAdmin seeds the first administrator when the user table is empty.
type BootstrapAdmin struct {
	UID         string
	Password    string
	DisplayName string
}

// EnsureBootstrapAdmin creates the bootstrap admin when Count==0 and UID is set.
func EnsureBootstrapAdmin(ctx context.Context, store Store, h auth.PasswordHasher, b BootstrapAdmin, logger *slog.Logger) error {
	n, err := store.Count(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if b.UID == "" {
		return ErrNoUsers
	}
	hash, err := h.Hash(b.Password)
	if err != nil {
		return err
	}
	display := b.DisplayName
	if display == "" {
		display = b.UID
	}
	u := &User{
		UID:          b.UID,
		DisplayName:  display,
		PasswordHash: hash,
		Enabled:      true,
	}
	if err := store.Create(ctx, u); err != nil {
		return err
	}
	if logger != nil {
		logger.InfoContext(ctx, "bootstrap admin created", slog.String("uid", b.UID))
	}
	return nil
}
