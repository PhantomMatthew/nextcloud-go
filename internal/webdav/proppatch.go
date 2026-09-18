package webdav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var errBadPropPatch = errors.New("webdav: invalid propertyupdate")

func (h *Handler) proppatch(w http.ResponseWriter, r *http.Request) {
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
		return
	}
	ops, err := parsePropPatch(r.Body)
	if err != nil || len(ops) == 0 {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if !h.checkLock(w, r, user, sub) {
		return
	}
	patcher, ok := h.FS.(PropPatchFS)
	if !ok {
		h.methodNotAllowed(w, r)
		return
	}
	results, err := patcher.PatchProps(r.Context(), user, sub, ops)
	if err != nil {
		writeFSError(w, err)
		return
	}
	href := h.hrefPrefix(user) + strings.TrimSuffix(sub, "/")
	if strings.HasSuffix(sub, "/") && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	var buf strings.Builder
	writePropPatchMultistatus(&buf, href, results)
	w.Header().Set("Content-Type", contentTypeXML)
	w.WriteHeader(StatusMultiStatus)
	_, err = io.WriteString(w, buf.String())
	if err != nil {
		return
	}
}

func parsePropPatch(r io.Reader) ([]PropPatchOp, error) {
	dec := xml.NewDecoder(r)
	var ops []PropPatchOp
	var inUpdate, inSet, inRemove, inProp bool
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
			switch t.Name.Local {
			case "propertyupdate":
				inUpdate = true
			case "set":
				if inUpdate {
					inSet = true
				}
			case "remove":
				if inUpdate {
					inRemove = true
				}
			case "prop":
				if inSet || inRemove {
					inProp = true
				}
			default:
				if inProp && (inSet || inRemove) {
					var inner string
					if err := dec.DecodeElement(&inner, &t); err != nil {
						return nil, err
					}
					ops = append(ops, PropPatchOp{
						Remove: inRemove,
						Space:  t.Name.Space,
						Name:   t.Name.Local,
						Value:  strings.TrimSpace(inner),
					})
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "propertyupdate":
				inUpdate = false
			case "set":
				inSet = false
			case "remove":
				inRemove = false
			case "prop":
				inProp = false
			}
		}
	}
	if len(ops) == 0 {
		return nil, errBadPropPatch
	}
	return ops, nil
}

func writePropPatchMultistatus(buf *strings.Builder, href string, results []PropPatchResult) {
	buf.WriteString(xmlHeader)
	buf.WriteString(multistatusNS)
	buf.WriteString(`<d:response>`)
	buf.WriteString(`<d:href>`)
	buf.WriteString(xmlEscape(href))
	buf.WriteString(`</d:href>`)
	byStatus := make(map[int][]PropPatchResult)
	var order []int
	for _, res := range results {
		if _, ok := byStatus[res.Status]; !ok {
			order = append(order, res.Status)
		}
		byStatus[res.Status] = append(byStatus[res.Status], res)
	}
	for _, st := range order {
		buf.WriteString(`<d:propstat><d:prop>`)
		for _, res := range byStatus[st] {
			buf.WriteString(emptyPropXML(res.Space, res.Name))
		}
		buf.WriteString(`</d:prop>`)
		fmt.Fprintf(buf, `<d:status>HTTP/1.1 %d %s</d:status>`, st, http.StatusText(st))
		buf.WriteString(`</d:propstat>`)
	}
	buf.WriteString(`</d:response>`)
	buf.WriteString(`</d:multistatus>` + "\n")
}

func emptyPropXML(space, name string) string {
	local := xmlEscape(name)
	switch space {
	case "http://owncloud.org/ns":
		return `<oc:` + local + `/>`
	case "http://nextcloud.org/ns":
		return `<nc:` + local + `/>`
	default:
		return `<d:` + local + `/>`
	}
}
