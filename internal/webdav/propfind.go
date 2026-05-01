package webdav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
)

const (
	xmlHeader     = `<?xml version="1.0"?>` + "\n"
	multistatusNS = `<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`
	httpRFC1123   = "Mon, 02 Jan 2006 15:04:05 GMT"
)

type PropfindContext struct {
	BaseHref   string
	InstanceID string
}

func WriteMultistatus(buf *bytes.Buffer, ctx PropfindContext, entries []*Entry) {
	buf.WriteString(xmlHeader)
	buf.WriteString(multistatusNS)
	for _, e := range entries {
		writeResponse(buf, ctx, e)
	}
	buf.WriteString(`</d:multistatus>` + "\n")
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
	if e.IsDir {
		buf.WriteString(`<d:resourcetype><d:collection/></d:resourcetype>`)
	} else {
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

	fmt.Fprintf(buf, `<oc:id>%s</oc:id>`, FileID(e.NumericID, ctx.InstanceID))
	fmt.Fprintf(buf, `<oc:fileid>%s</oc:fileid>`, FileID(e.NumericID, ctx.InstanceID))
	fmt.Fprintf(buf, `<oc:permissions>%s</oc:permissions>`, PermissionString(e.Permissions, e.IsDir, e.Shareable, e.Mounted, e.Shared))

	if e.IsDir {
		fmt.Fprintf(buf, `<oc:size>%d</oc:size>`, e.Size)
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
