package users

import (
	"context"
	"errors"
	"log/slog"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

// AdminGroupGID is Nextcloud's instance-administrators group: its members
// are instance admins (ADR-0080), the convention the console gate and
// NC-imported instances both rely on.
const AdminGroupGID = "admin"

// BootstrapAdmin seeds the first administrator when the user table is empty.
type BootstrapAdmin struct {
	UID         string
	Password    string
	DisplayName string
}

// EnsureBootstrapAdmin creates the bootstrap admin when Count==0 and UID is
// set. On that same empty-database path only it also guarantees the admin
// group exists and the bootstrap admin is a member — Nextcloud's convention
// for instance administrators (ADR-0080). Existing installs are untouched.
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
	if err := ensureAdminMembership(ctx, store, b.UID); err != nil {
		return err
	}
	if logger != nil {
		logger.InfoContext(ctx, "bootstrap admin created", slog.String("uid", b.UID))
	}
	return nil
}

// ensureAdminMembership creates the admin group when missing and adds uid
// unless already a member; a pre-existing membership is not an error.
func ensureAdminMembership(ctx context.Context, store Store, uid string) error {
	if _, err := store.GetGroupByGID(ctx, AdminGroupGID); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := store.CreateGroup(ctx, &Group{GID: AdminGroupGID, DisplayName: AdminGroupGID}); err != nil && !errors.Is(err, ErrExists) {
			return err
		}
	}
	gids, err := store.UserGroupGIDs(ctx, uid)
	if err != nil {
		return err
	}
	for _, gid := range gids {
		if gid == AdminGroupGID {
			return nil
		}
	}
	return store.AddGroupMember(ctx, AdminGroupGID, uid)
}
