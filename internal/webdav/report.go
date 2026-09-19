package webdav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (h *Handler) report(w http.ResponseWriter, r *http.Request) {
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
		return
	}
	reporter, ok := h.FS.(ReportFS)
	if !ok {
		h.methodNotAllowed(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	name := reportName(body)
	if name == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	entries, err := reporter.Report(r.Context(), user, sub, ReportRequest{Name: name, Body: body})
	if err != nil {
		writeFSError(w, err)
		return
	}
	baseHref := h.hrefPrefix(user)
	if !strings.HasSuffix(baseHref, "/") {
		baseHref += "/"
	}
	pctx := PropfindContext{
		BaseHref:         baseHref,
		InstanceID:       h.InstanceID,
		OwnerID:          user,
		OwnerDisplayName: h.ownerDisplayName(user),
		CalDAV:           h.emitCalDAV(),
		CardDAV:          h.emitCardDAV(),
	}
	var buf bytes.Buffer
	WriteReportMultistatus(&buf, pctx, entries)
	w.Header().Set("Content-Type", contentTypeXML)
	w.WriteHeader(StatusMultiStatus)
	_, _ = w.Write(buf.Bytes())
}

func (h *Handler) mkcalendar(w http.ResponseWriter, r *http.Request) {
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
		return
	}
	maker, ok := h.FS.(CalendarMkdirFS)
	if !ok {
		h.methodNotAllowed(w, r)
		return
	}
	props, err := parseMKCalendar(r.Body)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	entry, err := maker.MkCalendar(r.Context(), user, sub, props)
	if err != nil {
		writeFSError(w, err)
		return
	}
	w.Header().Set(HeaderOCETag, `"`+entry.ETag+`"`)
	w.Header().Set(HeaderOCFileID, FileID(entry.NumericID, h.InstanceID))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
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
		case "calendar-query", "calendar-multiget", "addressbook-query", "addressbook-multiget":
			return se.Name.Local
		}
	}
}

func parseMKCalendar(r io.Reader) (map[string]string, error) {
	props := map[string]string{}
	if r == nil {
		return props, nil
	}
	dec := xml.NewDecoder(r)
	var inSet, inProp bool
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return props, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "set":
				inSet = true
			case "prop":
				if inSet {
					inProp = true
				}
			default:
				if inProp {
					var inner string
					if err := dec.DecodeElement(&inner, &t); err != nil {
						return nil, err
					}
					props[t.Name.Local] = strings.TrimSpace(inner)
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "set":
				inSet = false
			case "prop":
				inProp = false
			}
		}
	}
}

// WriteReportMultistatus writes a CalDAV/CardDAV REPORT 207 body.
func WriteReportMultistatus(buf *bytes.Buffer, ctx PropfindContext, entries []*Entry) {
	buf.WriteString(xmlHeader)
	buf.WriteString(multistatusOpen(ctx.CalDAV, ctx.CardDAV))
	for _, e := range entries {
		writeReportResponse(buf, ctx, e)
	}
	buf.WriteString(`</d:multistatus>` + "\n")
}

func writeReportResponse(buf *bytes.Buffer, ctx PropfindContext, e *Entry) {
	buf.WriteString(`<d:response>`)
	buf.WriteString(`<d:href>`)
	buf.WriteString(xmlEscape(buildHref(ctx.BaseHref, e)))
	buf.WriteString(`</d:href>`)
	if e.Status == http.StatusNotFound {
		buf.WriteString(`<d:propstat><d:prop/><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat>`)
		buf.WriteString(`</d:response>`)
		return
	}
	buf.WriteString(`<d:propstat><d:prop>`)
	fmt.Fprintf(buf, `<d:getetag>&quot;%s&quot;</d:getetag>`, xmlEscape(e.ETag))
	if e.CalendarData != "" {
		fmt.Fprintf(buf, `<cal:calendar-data>%s</cal:calendar-data>`, xmlEscape(e.CalendarData))
	}
	if e.AddressData != "" {
		fmt.Fprintf(buf, `<card:address-data>%s</card:address-data>`, xmlEscape(e.AddressData))
	}
	buf.WriteString(`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>`)
	buf.WriteString(`</d:response>`)
}
