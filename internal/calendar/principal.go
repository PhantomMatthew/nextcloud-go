package calendar

import (
	"context"
	"io"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// PrincipalDAV implements PROPFIND-only principals/users/{uid}.
type PrincipalDAV struct {
	Users users.Store
	Clock func() time.Time
}

func (d *PrincipalDAV) now() time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	return time.Now().UTC()
}

func (d *PrincipalDAV) Stat(ctx context.Context, user, p string) (*webdav.Entry, error) {
	u, err := d.Users.GetByUID(ctx, user)
	if err != nil {
		return nil, mapErr(ErrNotFound)
	}
	np := stringsTrimPath(p)
	if np != "/" {
		return nil, webdav.ErrNotFound
	}
	return d.entry(u), nil
}

func (d *PrincipalDAV) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	if _, err := d.Stat(ctx, user, p); err != nil {
		return nil, err
	}
	return nil, nil
}

func (d *PrincipalDAV) Read(context.Context, string, string) (io.ReadCloser, *webdav.Entry, error) {
	return nil, nil, webdav.ErrMethodNotAllowed
}

func (d *PrincipalDAV) Write(context.Context, string, string, io.Reader, *time.Time) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (d *PrincipalDAV) Mkdir(context.Context, string, string) (*webdav.Entry, error) {
	return nil, webdav.ErrMethodNotAllowed
}

func (d *PrincipalDAV) Remove(context.Context, string, string) error {
	return webdav.ErrMethodNotAllowed
}

func (d *PrincipalDAV) Move(context.Context, string, string, string, string, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (d *PrincipalDAV) Copy(context.Context, string, string, string, string, bool, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (d *PrincipalDAV) entry(u *users.User) *webdav.Entry {
	name := u.DisplayName
	if name == "" {
		name = u.UID
	}
	return &webdav.Entry{
		Path:                 "/",
		IsDir:                true,
		IsPrincipal:          true,
		ETag:                 "principal",
		ModTime:              d.now(),
		NumericID:            numericID(u.ID),
		Permissions:          webdav.PermRead,
		ContentType:          "httpd/unix-directory",
		DisplayName:          name,
		CurrentUserPrincipal: "/remote.php/dav/principals/users/" + u.UID + "/",
		CalendarHomeSet:      "/remote.php/dav/calendars/" + u.UID + "/",
		AddressbookHomeSet:   "/remote.php/dav/addressbooks/users/" + u.UID + "/",
	}
}

func stringsTrimPath(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return p
}
