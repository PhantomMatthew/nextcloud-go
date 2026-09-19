package contacts

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func (d *DAV) Report(ctx context.Context, user, p string, req webdav.ReportRequest) ([]*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return nil, err
	}
	if objURI != "" {
		return nil, webdav.ErrBadRequest
	}
	kind := req.Name
	if kind == "" {
		kind = reportName(req.Body)
	}
	switch kind {
	case "addressbook-query":
		return d.query(ctx, u.ID, bookURI)
	case "addressbook-multiget":
		return d.multiget(ctx, user, u.ID, req.Body)
	default:
		return nil, webdav.ErrBadRequest
	}
}

func (d *DAV) query(ctx context.Context, userID int64, bookURI string) ([]*webdav.Entry, error) {
	var books []Addressbook
	if bookURI == "" {
		listed, err := d.Store.ListBooks(ctx, userID)
		if err != nil {
			return nil, err
		}
		books = listed
	} else {
		b, err := d.Store.GetBookByURI(ctx, userID, bookURI)
		if err != nil {
			return nil, mapErr(err)
		}
		books = []Addressbook{*b}
	}
	var out []*webdav.Entry
	for i := range books {
		objs, err := d.Store.ListObjects(ctx, userID, books[i].URI)
		if err != nil {
			return nil, mapErr(err)
		}
		for j := range objs {
			out = append(out, objectEntry(books[i].URI, &objs[j]))
		}
	}
	return out, nil
}

func (d *DAV) multiget(ctx context.Context, uid string, userID int64, body []byte) ([]*webdav.Entry, error) {
	hrefs := parseHrefs(body)
	out := make([]*webdav.Entry, 0, len(hrefs))
	for _, href := range hrefs {
		bookURI, objURI, ok := hrefToObject(href, uid)
		if !ok {
			out = append(out, &webdav.Entry{Path: href, Status: http.StatusNotFound})
			continue
		}
		obj, err := d.Store.GetObject(ctx, userID, bookURI, objURI)
		if err != nil {
			out = append(out, &webdav.Entry{Path: "/" + bookURI + "/" + objURI, Status: http.StatusNotFound})
			continue
		}
		out = append(out, objectEntry(bookURI, obj))
	}
	return out, nil
}

func reportName(body []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "addressbook-query", "addressbook-multiget":
			return se.Name.Local
		}
	}
}

func parseHrefs(body []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var hrefs []string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return hrefs
		}
		if err != nil {
			return hrefs
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "href" {
			continue
		}
		var v string
		if err := dec.DecodeElement(&v, &se); err != nil {
			continue
		}
		hrefs = append(hrefs, strings.TrimSpace(v))
	}
}

func hrefToObject(href, uid string) (bookURI, objURI string, ok bool) {
	p := href
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		p = u.Path
	}
	prefix := "/remote.php/dav/addressbooks/users/" + uid
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	bookURI, objURI, err := splitBookPath(rest)
	if err != nil || bookURI == "" || objURI == "" {
		return "", "", false
	}
	return bookURI, objURI, true
}
