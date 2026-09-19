package calendar

import (
	"context"
	"io"
	"path"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// RootDAV is PROPFIND on /remote.php/dav/ (OwnerUID mode).
type RootDAV struct {
	Users users.Store
	Clock func() time.Time
}

func (d *RootDAV) now() time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	return time.Now().UTC()
}

func (d *RootDAV) Stat(ctx context.Context, user, p string) (*webdav.Entry, error) {
	u, err := d.Users.GetByUID(ctx, user)
	if err != nil {
		return nil, mapErr(ErrNotFound)
	}
	np := path.Clean("/" + strings.Trim(p, "/"))
	switch np {
	case "/", ".":
		return d.rootEntry(u), nil
	case "/calendars", "/principals", "/files", "/addressbooks":
		return &webdav.Entry{
			Path:        np,
			IsDir:       true,
			ETag:        "0",
			ModTime:     d.now(),
			NumericID:   numericID(u.ID),
			Permissions: webdav.PermRead,
			ContentType: "httpd/unix-directory",
			DisplayName: strings.TrimPrefix(np, "/"),
		}, nil
	default:
		return nil, webdav.ErrNotFound
	}
}

func (d *RootDAV) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	u, err := d.Users.GetByUID(ctx, user)
	if err != nil {
		return nil, mapErr(ErrNotFound)
	}
	np := path.Clean("/" + strings.Trim(p, "/"))
	if np != "/" && np != "." {
		if _, err := d.Stat(ctx, user, p); err != nil {
			return nil, err
		}
		return nil, nil
	}
	kids := []string{"/calendars", "/principals", "/files", "/addressbooks"}
	out := make([]*webdav.Entry, 0, len(kids))
	for _, k := range kids {
		e, err := d.Stat(ctx, u.UID, k)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (d *RootDAV) Read(context.Context, string, string) (io.ReadCloser, *webdav.Entry, error) {
	return nil, nil, webdav.ErrMethodNotAllowed
}

func (d *RootDAV) Write(context.Context, string, string, io.Reader, *time.Time) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (d *RootDAV) Mkdir(context.Context, string, string) (*webdav.Entry, error) {
	return nil, webdav.ErrMethodNotAllowed
}

func (d *RootDAV) Remove(context.Context, string, string) error {
	return webdav.ErrMethodNotAllowed
}

func (d *RootDAV) Move(context.Context, string, string, string, string, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (d *RootDAV) Copy(context.Context, string, string, string, string, bool, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (d *RootDAV) rootEntry(u *users.User) *webdav.Entry {
	return &webdav.Entry{
		Path:                 "/",
		IsDir:                true,
		ETag:                 "dav-root",
		ModTime:              d.now(),
		NumericID:            numericID(u.ID),
		Permissions:          webdav.PermRead,
		ContentType:          "httpd/unix-directory",
		DisplayName:          u.UID,
		CurrentUserPrincipal: "/remote.php/dav/principals/users/" + u.UID + "/",
		CalendarHomeSet:      "/remote.php/dav/calendars/" + u.UID + "/",
		AddressbookHomeSet:   "/remote.php/dav/addressbooks/users/" + u.UID + "/",
	}
}
