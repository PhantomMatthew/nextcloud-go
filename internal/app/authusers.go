package app

import (
	"context"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

type authUsers struct {
	store users.Store
}

func (a authUsers) GetByUID(ctx context.Context, uid string) (*auth.UserInfo, error) {
	u, err := a.store.GetByUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	return userInfo(u), nil
}

func (a authUsers) GetByID(ctx context.Context, id int64) (*auth.UserInfo, error) {
	u, err := a.store.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return userInfo(u), nil
}

func userInfo(u *users.User) *auth.UserInfo {
	return &auth.UserInfo{ID: u.ID, UID: u.UID, DisplayName: u.DisplayName, Enabled: u.Enabled}
}
