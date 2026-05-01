package webdav

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

const (
	StatusMultiStatus = 207

	HeaderDepth   = "Depth"
	HeaderDAV     = "DAV"
	HeaderAllow   = "Allow"
	HeaderMSAuthor = "MS-Author-Via"

	davCompliance    = "1, 3, extended-mkcol"
	allowedMethods   = "OPTIONS, GET, HEAD, PROPFIND"
	contentTypeXML   = "application/xml; charset=utf-8"
)

type Handler struct {
	Prefix     string
	FS         FS
	InstanceID string
}

func NewHandler(prefix string, fs FS, instanceID string) *Handler {
	if prefix == "" || prefix[0] != '/' {
		panic("webdav: prefix must start with /")
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Handler{Prefix: prefix, FS: fs, InstanceID: instanceID}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "OPTIONS":
		h.options(w, r)
	case "PROPFIND":
		h.propfind(w, r)
	case "GET", "HEAD":
		h.notFound(w, r)
	default:
		h.methodNotAllowed(w, r)
	}
}

func (h *Handler) options(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(HeaderDAV, davCompliance)
	w.Header().Set(HeaderAllow, allowedMethods)
	w.Header().Set(HeaderMSAuthor, "DAV")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) propfind(w http.ResponseWriter, r *http.Request) {
	user, sub, ok := h.parsePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	principal, ok := auth.UserFromContext(r.Context())
	if !ok || principal.UID != user {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	depth := normalizeDepth(r.Header.Get(HeaderDepth))

	root, err := h.FS.Stat(r.Context(), user, sub)
	if err != nil {
		writeFSError(w, err)
		return
	}

	entries := []*Entry{root}
	if depth != "0" && root.IsDir {
		children, err := h.FS.List(r.Context(), user, sub)
		if err != nil {
			writeFSError(w, err)
			return
		}
		entries = append(entries, children...)
	}

	baseHref := h.Prefix + user + strings.TrimSuffix(sub, "/")
	if root.IsDir && !strings.HasSuffix(baseHref, "/") {
		baseHref += "/"
	}

	var buf bytes.Buffer
	WriteMultistatus(&buf, PropfindContext{
		BaseHref:   baseHref,
		InstanceID: h.InstanceID,
	}, entries)

	w.Header().Set("Content-Type", contentTypeXML)
	w.WriteHeader(StatusMultiStatus)
	_, _ = w.Write(buf.Bytes())
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Not Found", http.StatusNotFound)
}

func (h *Handler) methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(HeaderAllow, allowedMethods)
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

func (h *Handler) parsePath(p string) (user, sub string, ok bool) {
	if !strings.HasPrefix(p, h.Prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, h.Prefix)
	if rest == "" {
		return "", "", false
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return rest, "/", true
	}
	user = rest[:slash]
	sub = rest[slash:]
	if user == "" {
		return "", "", false
	}
	if sub == "" {
		sub = "/"
	}
	return user, sub, true
}

func normalizeDepth(d string) string {
	switch d {
	case "0":
		return "0"
	case "1":
		return "1"
	case "infinity", "":
		return "1"
	}
	return "1"
}

func writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "Not Found", http.StatusNotFound)
	case errors.Is(err, ErrForbidden):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, ErrNotDir):
		http.Error(w, "Conflict", http.StatusConflict)
	default:
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
	_ = context.Canceled
}
