package contacts

import (
	"context"
	"encoding/xml"
	"errors"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// csShare is the sharing POST body (calendarserver.org/ns share), the same
// XML shape CardDAV clients POST to an addressbook collection.
type csShare struct {
	XMLName xml.Name `xml:"share"`
	Sets    []struct {
		Href      string    `xml:"href"`
		ReadWrite *struct{} `xml:"read-write"`
	} `xml:"set"`
	Removes []struct {
		Href string `xml:"href"`
	} `xml:"remove"`
}

// Share implements webdav.ShareFS: POST cs:share on an own addressbook.
// Shares take effect immediately; invite notifications are not sent.
func (d *DAV) Share(ctx context.Context, user, p string, body []byte) error {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return err
	}
	if bookURI == "" || objURI != "" {
		return webdav.ErrMethodNotAllowed
	}
	rb, err := d.resolveBook(ctx, u, bookURI)
	if err != nil {
		return mapErr(err)
	}
	if rb.shared {
		return mapErr(ErrForbidden)
	}
	var req csShare
	if err := xml.Unmarshal(body, &req); err != nil {
		return webdav.ErrBadRequest
	}
	if len(req.Sets) == 0 && len(req.Removes) == 0 {
		return webdav.ErrBadRequest
	}
	for _, set := range req.Sets {
		uid, err := principalUID(set.Href)
		if err != nil {
			return err
		}
		target, err := d.Users.GetByUID(ctx, uid)
		if err != nil {
			return webdav.ErrBadRequest
		}
		if target.ID == u.ID {
			return webdav.ErrBadRequest
		}
		access := ShareAccessRead
		if set.ReadWrite != nil {
			access = ShareAccessReadWrite
		}
		if err := d.Store.UpsertAddressbookShare(ctx, rb.book.ID, target.ID, access); err != nil {
			return mapErr(err)
		}
	}
	for _, rm := range req.Removes {
		uid, err := principalUID(rm.Href)
		if err != nil {
			return err
		}
		target, err := d.Users.GetByUID(ctx, uid)
		if err != nil {
			return webdav.ErrBadRequest
		}
		if err := d.Store.DeleteAddressbookShare(ctx, rb.book.ID, target.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return mapErr(err)
		}
	}
	return nil
}

// principalUID extracts the uid from a principal href
// (/remote.php/dav/principals/users/{uid}/).
func principalUID(href string) (string, error) {
	href = strings.Trim(href, "/")
	const marker = "principals/users/"
	i := strings.Index(href, marker)
	if i < 0 {
		return "", webdav.ErrBadRequest
	}
	uid := href[i+len(marker):]
	if uid == "" || strings.Contains(uid, "/") {
		return "", webdav.ErrBadRequest
	}
	return uid, nil
}
