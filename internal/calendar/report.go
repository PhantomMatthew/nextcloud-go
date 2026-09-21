package calendar

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func (d *DAV) Report(ctx context.Context, user, p string, req webdav.ReportRequest) ([]*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	calURI, objURI, err := splitCalPath(p)
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
	case "calendar-query":
		comp, start, end := parseCompFilter(req.Body)
		return d.query(ctx, u.ID, calURI, comp, start, end)
	case "calendar-multiget":
		return d.multiget(ctx, user, u.ID, req.Body)
	default:
		return nil, webdav.ErrBadRequest
	}
}

func (d *DAV) query(ctx context.Context, userID int64, calURI, component string, start, end time.Time) ([]*webdav.Entry, error) {
	var cals []Calendar
	if calURI == "" {
		listed, err := d.Store.ListCalendars(ctx, userID)
		if err != nil {
			return nil, err
		}
		cals = listed
	} else {
		c, err := d.Store.GetCalendarByURI(ctx, userID, calURI)
		if err != nil {
			return nil, mapErr(err)
		}
		cals = []Calendar{*c}
	}
	var out []*webdav.Entry
	for i := range cals {
		objs, err := d.Store.ObjectsInRange(ctx, userID, cals[i].URI, start, end)
		if err != nil {
			return nil, mapErr(err)
		}
		for j := range objs {
			if component != "" && objs[j].Component != component {
				continue
			}
			out = append(out, objectEntry(cals[i].URI, &objs[j]))
		}
	}
	return out, nil
}

func (d *DAV) multiget(ctx context.Context, uid string, userID int64, body []byte) ([]*webdav.Entry, error) {
	hrefs := parseHrefs(body)
	out := make([]*webdav.Entry, 0, len(hrefs))
	for _, href := range hrefs {
		calURI, objURI, ok := hrefToObject(href, uid)
		if !ok {
			out = append(out, &webdav.Entry{Path: href, Status: http.StatusNotFound})
			continue
		}
		obj, err := d.Store.GetObject(ctx, userID, calURI, objURI)
		if err != nil {
			out = append(out, &webdav.Entry{Path: "/" + calURI + "/" + objURI, Status: http.StatusNotFound})
			continue
		}
		out = append(out, objectEntry(calURI, obj))
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
		case "calendar-query", "calendar-multiget":
			return se.Name.Local
		}
	}
}

// parseCompFilter returns the innermost comp-filter component name
// (e.g. VEVENT, VTODO) and the time-range of a calendar-query body.
// An empty component means no component filter (all components match).
func parseCompFilter(body []byte) (component string, start, end time.Time) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return component, start, end
		}
		switch e := tok.(type) {
		case xml.StartElement:
			switch e.Name.Local {
			case "comp-filter":
				depth++
				if depth >= 2 {
					for _, a := range e.Attr {
						if a.Name.Local == "name" {
							component = strings.ToUpper(a.Value)
						}
					}
				}
			case "time-range":
				for _, a := range e.Attr {
					switch a.Name.Local {
					case "start":
						if t, err := time.Parse("20060102T150405Z", a.Value); err == nil {
							start = t.UTC()
						}
					case "end":
						if t, err := time.Parse("20060102T150405Z", a.Value); err == nil {
							end = t.UTC()
						}
					}
				}
			}
		case xml.EndElement:
			if e.Name.Local == "comp-filter" && depth > 0 {
				depth--
			}
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

func hrefToObject(href, uid string) (calURI, objURI string, ok bool) {
	p := href
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		p = u.Path
	}
	prefix := "/remote.php/dav/calendars/" + uid
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	calURI, objURI, err := splitCalPath(rest)
	if err != nil || calURI == "" || objURI == "" {
		return "", "", false
	}
	return calURI, objURI, true
}
