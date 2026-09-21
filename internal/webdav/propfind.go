package webdav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	xmlHeader     = `<?xml version="1.0"?>` + "\n"
	multistatusNS = `<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`
	httpRFC1123   = "Mon, 02 Jan 2006 15:04:05 GMT"
)

type PropfindContext struct {
	BaseHref         string
	InstanceID       string
	OwnerID          string
	OwnerDisplayName string
	EmitQuota        bool
	QuotaUsed        int64
	QuotaAvailable   int64
	EmitFavorite     bool
	EmitLocks        bool
	CalDAV           bool
	CardDAV          bool
}

func WriteMultistatus(buf *bytes.Buffer, ctx PropfindContext, entries []*Entry) {
	buf.WriteString(xmlHeader)
	buf.WriteString(multistatusOpen(ctx.CalDAV, ctx.CardDAV))
	for _, e := range entries {
		writeResponse(buf, ctx, e)
	}
	buf.WriteString(`</d:multistatus>` + "\n")
}

func multistatusOpen(caldav, carddav bool) string {
	if !caldav && !carddav {
		return multistatusNS
	}
	ns := `<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns"`
	if caldav {
		ns += ` xmlns:cal="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/" xmlns:apple="http://apple.com/ns/ical/"`
	}
	if carddav {
		ns += ` xmlns:card="urn:ietf:params:xml:ns:carddav"`
		if !caldav {
			ns += ` xmlns:cs="http://calendarserver.org/ns/"`
		}
	}
	return ns + `>`
}

func writeResponse(buf *bytes.Buffer, ctx PropfindContext, e *Entry) {
	buf.WriteString(`<d:response>`)
	buf.WriteString(`<d:href>`)
	buf.WriteString(xmlEscape(buildHref(ctx.BaseHref, e)))
	buf.WriteString(`</d:href>`)
	buf.WriteString(`<d:propstat>`)
	buf.WriteString(`<d:prop>`)
	writeProps(buf, ctx, e)
	buf.WriteString(`</d:prop>`)
	buf.WriteString(`<d:status>HTTP/1.1 200 OK</d:status>`)
	buf.WriteString(`</d:propstat>`)
	buf.WriteString(`</d:response>`)
}

func writeProps(buf *bytes.Buffer, ctx PropfindContext, e *Entry) {
	switch {
	case e.IsPrincipal:
		buf.WriteString(`<d:resourcetype><d:principal/><d:collection/></d:resourcetype>`)
	case e.IsCalendar:
		buf.WriteString(`<d:resourcetype><d:collection/><cal:calendar/></d:resourcetype>`)
	case e.IsAddressbook:
		buf.WriteString(`<d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>`)
	case e.IsDir:
		buf.WriteString(`<d:resourcetype><d:collection/></d:resourcetype>`)
	default:
		buf.WriteString(`<d:resourcetype/>`)
	}

	fmt.Fprintf(buf, `<d:getetag>&quot;%s&quot;</d:getetag>`, xmlEscape(e.ETag))

	if !e.IsDir {
		fmt.Fprintf(buf, `<d:getcontentlength>%d</d:getcontentlength>`, e.Size)
		ct := e.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		fmt.Fprintf(buf, `<d:getcontenttype>%s</d:getcontenttype>`, xmlEscape(ct))
	}

	mod := e.ModTime.UTC().Format(httpRFC1123)
	fmt.Fprintf(buf, `<d:getlastmodified>%s</d:getlastmodified>`, mod)

	if ctx.EmitLocks {
		writeSupportedLock(buf)
		if e.LockToken != "" {
			writeLockDiscovery(buf, &LockInfo{
				Token:   e.LockToken,
				Owner:   e.LockOwner,
				Timeout: e.LockTimeout,
				Path:    e.Path,
			}, "")
		} else {
			buf.WriteString(`<d:lockdiscovery/>`)
		}
	}

	fmt.Fprintf(buf, `<oc:id>%s</oc:id>`, FileID(e.NumericID, ctx.InstanceID))
	fmt.Fprintf(buf, `<oc:fileid>%s</oc:fileid>`, FileID(e.NumericID, ctx.InstanceID))
	fmt.Fprintf(buf, `<oc:permissions>%s</oc:permissions>`, PermissionString(e.Permissions, e.IsDir, e.Shareable, e.Mounted, e.Shared))
	if ctx.EmitFavorite {
		fav := 0
		if e.Favorite == 1 {
			fav = 1
		}
		fmt.Fprintf(buf, `<oc:favorite>%d</oc:favorite>`, fav)
	}

	if e.IsDir {
		fmt.Fprintf(buf, `<oc:size>%d</oc:size>`, e.Size)
	}
	if e.Checksum != "" {
		fmt.Fprintf(buf, `<oc:checksums><oc:checksum>%s</oc:checksum></oc:checksums>`, xmlEscape(e.Checksum))
	}
	if e.TrashOriginal != "" {
		fmt.Fprintf(buf, `<oc:trashbin-original-location>%s</oc:trashbin-original-location>`, xmlEscape(e.TrashOriginal))
	}
	if e.TrashDeleted > 0 {
		fmt.Fprintf(buf, `<oc:trashbin-deletion-time>%d</oc:trashbin-deletion-time>`, e.TrashDeleted)
	}
	fmt.Fprintf(buf, `<oc:owner-id>%s</oc:owner-id>`, xmlEscape(ctx.OwnerID))
	fmt.Fprintf(buf, `<oc:owner-display-name>%s</oc:owner-display-name>`, xmlEscape(ctx.OwnerDisplayName))
	buf.WriteString(`<nc:is-encrypted>false</nc:is-encrypted>`)
	buf.WriteString(`<nc:mount-type></nc:mount-type>`)
	if ctx.EmitQuota && (e.Path == "" || e.Path == "/") {
		fmt.Fprintf(buf, `<d:quota-used-bytes>%d</d:quota-used-bytes>`, ctx.QuotaUsed)
		fmt.Fprintf(buf, `<d:quota-available-bytes>%d</d:quota-available-bytes>`, ctx.QuotaAvailable)
	}
	if e.DisplayName != "" {
		fmt.Fprintf(buf, `<d:displayname>%s</d:displayname>`, xmlEscape(e.DisplayName))
	}
	if e.CurrentUserPrincipal != "" {
		fmt.Fprintf(buf, `<d:current-user-principal><d:href>%s</d:href></d:current-user-principal>`, xmlEscape(e.CurrentUserPrincipal))
	}
	if e.CalendarHomeSet != "" {
		fmt.Fprintf(buf, `<cal:calendar-home-set><d:href>%s</d:href></cal:calendar-home-set>`, xmlEscape(e.CalendarHomeSet))
	}
	if e.AddressbookHomeSet != "" {
		fmt.Fprintf(buf, `<card:addressbook-home-set><d:href>%s</d:href></card:addressbook-home-set>`, xmlEscape(e.AddressbookHomeSet))
	}
	if e.IsCalendar {
		if e.CTag != "" {
			fmt.Fprintf(buf, `<cs:getctag>&quot;%s&quot;</cs:getctag>`, xmlEscape(e.CTag))
			fmt.Fprintf(buf, `<d:sync-token>https://nextcloud-go/sync/%s</d:sync-token>`, xmlEscape(e.CTag))
		}
		buf.WriteString(`<cal:supported-calendar-component-set><cal:comp name="VEVENT"/><cal:comp name="VTODO"/></cal:supported-calendar-component-set>`)
		if e.CalendarColor != "" {
			fmt.Fprintf(buf, `<apple:calendar-color>%s</apple:calendar-color>`, xmlEscape(e.CalendarColor))
		}
		fmt.Fprintf(buf, `<apple:calendar-order>%d</apple:calendar-order>`, e.CalendarOrder)
		enabled := 0
		if e.CalendarEnabled {
			enabled = 1
		}
		fmt.Fprintf(buf, `<oc:calendar-enabled>%d</oc:calendar-enabled>`, enabled)
		if e.CalendarDescription != "" {
			fmt.Fprintf(buf, `<cal:calendar-description>%s</cal:calendar-description>`, xmlEscape(e.CalendarDescription))
		}
	}
	if e.IsAddressbook && e.CTag != "" {
		fmt.Fprintf(buf, `<cs:getctag>&quot;%s&quot;</cs:getctag>`, xmlEscape(e.CTag))
		fmt.Fprintf(buf, `<d:sync-token>https://nextcloud-go/sync/%s</d:sync-token>`, xmlEscape(e.CTag))
	}
}

func buildHref(base string, e *Entry) string {
	p := e.Path
	if p == "" || p == "/" {
		if e.IsDir && !strings.HasSuffix(base, "/") {
			return base + "/"
		}
		return base
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	out := strings.TrimRight(base, "/") + "/" + strings.Join(segs, "/")
	if e.IsDir && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ParseMultistatus reads a DAV 207 body into entries keyed by href basename.
func ParseMultistatus(r io.Reader) ([]*Entry, error) {
	if r == nil {
		return nil, fmt.Errorf("webdav: nil multistatus")
	}
	dec := xml.NewDecoder(r)
	var out []*Entry
	var cur *Entry
	inResType := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			switch local {
			case "response":
				cur = &Entry{}
			case "href":
				if cur == nil {
					cur = &Entry{}
				}
				var href string
				if err := dec.DecodeElement(&href, &t); err != nil {
					return nil, err
				}
				cur.Path = hrefToPath(href)
			case "resourcetype":
				inResType = true
			case "collection":
				if inResType && cur != nil {
					cur.IsDir = true
				}
			case "getcontentlength":
				if cur == nil {
					continue
				}
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				n, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
				if perr == nil {
					cur.Size = n
				}
			case "getetag":
				if cur == nil {
					continue
				}
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				cur.ETag = strings.Trim(s, `"'`)
			case "getcontenttype":
				if cur == nil {
					continue
				}
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				cur.ContentType = strings.TrimSpace(s)
			case "getlastmodified":
				if cur == nil {
					continue
				}
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				if tm, perr := http.ParseTime(strings.TrimSpace(s)); perr == nil {
					cur.ModTime = tm
				}
			}
		case xml.EndElement:
			switch strings.ToLower(t.Name.Local) {
			case "response":
				if cur != nil {
					out = append(out, cur)
					cur = nil
				}
			case "resourcetype":
				inResType = false
			}
		}
	}
	return out, nil
}

func hrefToPath(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return "/"
	}
	u, err := url.Parse(href)
	p := href
	if err == nil && u.Path != "" {
		p = u.Path
	}
	if unesc, uerr := url.PathUnescape(p); uerr == nil {
		p = unesc
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	base := p[strings.LastIndex(p, "/")+1:]
	if base == "" || base == "webdav" {
		return "/"
	}
	return "/" + base
}
