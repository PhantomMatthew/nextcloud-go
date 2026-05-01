package webdav

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

const (
	StatusMultiStatus = 207

	HeaderDepth     = "Depth"
	HeaderDAV       = "DAV"
	HeaderAllow     = "Allow"
	HeaderMSAuthor  = "MS-Author-Via"
	HeaderIfMatch   = "If-Match"
	HeaderIfNoneMatch = "If-None-Match"
	HeaderOCMtime   = "X-OC-Mtime"
	HeaderOCChunked = "OC-Chunked"
	HeaderOCETag    = "OC-ETag"
	HeaderOCFileID  = "OC-FileId"

	davCompliance  = "1, 3, extended-mkcol"
	allowedMethods = "OPTIONS, GET, HEAD, PROPFIND, PUT, MKCOL, DELETE, MOVE, COPY"
	contentTypeXML = "application/xml; charset=utf-8"

	HeaderDestination = "Destination"
	HeaderOverwrite   = "Overwrite"
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
	case "GET":
		h.get(w, r, true)
	case "HEAD":
		h.get(w, r, false)
	case "PUT":
		h.put(w, r)
	case "MKCOL":
		h.mkcol(w, r)
	case "DELETE":
		h.delete(w, r)
	case "MOVE":
		h.moveOrCopy(w, r, false)
	case "COPY":
		h.moveOrCopy(w, r, true)
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

func (h *Handler) get(w http.ResponseWriter, r *http.Request, writeBody bool) {
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

	rc, entry, err := h.FS.Read(r.Context(), user, sub)
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", entry.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	w.Header().Set("Last-Modified", entry.ModTime.UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", `"`+entry.ETag+`"`)
	w.Header().Set(HeaderOCETag, `"`+entry.ETag+`"`)
	w.Header().Set(HeaderOCFileID, FileID(entry.NumericID, h.InstanceID))

	if !writeBody {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
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

	if r.Header.Get(HeaderOCChunked) != "" {
		http.Error(w, "Not Implemented", http.StatusNotImplemented)
		return
	}

	existing, statErr := h.FS.Stat(r.Context(), user, sub)
	if statErr != nil && !errors.Is(statErr, ErrNotFound) {
		writeFSError(w, statErr)
		return
	}
	exists := statErr == nil

	if ifMatch := r.Header.Get(HeaderIfMatch); ifMatch != "" {
		if !exists || !etagMatches(ifMatch, existing.ETag) {
			http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
			return
		}
	}
	if inm := r.Header.Get(HeaderIfNoneMatch); inm != "" {
		if inm == "*" && exists {
			http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
			return
		}
		if exists && etagMatches(inm, existing.ETag) {
			http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
			return
		}
	}

	var mtimePtr *time.Time
	mtimeAccepted := false
	if v := r.Header.Get(HeaderOCMtime); v != "" {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			t := time.Unix(secs, 0).UTC()
			mtimePtr = &t
			mtimeAccepted = true
		}
	}

	body := r.Body
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if r.ContentLength >= 0 && int64(len(data)) != r.ContentLength {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	entry, created, err := h.FS.Write(r.Context(), user, sub, bytes.NewReader(data), mtimePtr)
	if err != nil {
		writeFSError(w, err)
		return
	}

	w.Header().Set("ETag", `"`+entry.ETag+`"`)
	w.Header().Set(HeaderOCETag, `"`+entry.ETag+`"`)
	w.Header().Set(HeaderOCFileID, FileID(entry.NumericID, h.InstanceID))
	w.Header().Set("Last-Modified", entry.ModTime.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", "0")
	if mtimeAccepted {
		w.Header().Set(HeaderOCMtime, "accepted")
	}
	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
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

func etagMatches(header, etag string) bool {
	for _, raw := range strings.Split(header, ",") {
		v := strings.TrimSpace(raw)
		v = strings.TrimPrefix(v, "W/")
		v = strings.Trim(v, `"`)
		if v == etag || v == "*" {
			return true
		}
	}
	return false
}

func writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "Not Found", http.StatusNotFound)
	case errors.Is(err, ErrForbidden):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, ErrLocked):
		http.Error(w, "Locked", http.StatusLocked)
	case errors.Is(err, ErrExists):
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	case errors.Is(err, ErrNotDir), errors.Is(err, ErrIsDir), errors.Is(err, ErrParentMissing):
		http.Error(w, "Conflict", http.StatusConflict)
	default:
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *Handler) mkcol(w http.ResponseWriter, r *http.Request) {
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
	if r.ContentLength > 0 {
		http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
		return
	}
	if sub == "/" {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	entry, err := h.FS.Mkdir(r.Context(), user, sub)
	if err != nil {
		writeFSError(w, err)
		return
	}
	w.Header().Set(HeaderOCETag, `"`+entry.ETag+`"`)
	w.Header().Set(HeaderOCFileID, FileID(entry.NumericID, h.InstanceID))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
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
	if sub == "/" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := h.FS.Remove(r.Context(), user, sub); err != nil {
		writeFSError(w, err)
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) moveOrCopy(w http.ResponseWriter, r *http.Request, isCopy bool) {
	srcUser, srcSub, ok := h.parsePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	principal, ok := auth.UserFromContext(r.Context())
	if !ok || principal.UID != srcUser {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if srcSub == "/" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	dstUser, dstSub, derr := h.parseDestination(r)
	if derr != nil {
		http.Error(w, derr.Error(), http.StatusBadRequest)
		return
	}
	if dstSub == "/" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if dstUser != srcUser {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	overwrite := parseOverwrite(r)

	if isCopy {
		depth := r.Header.Get(HeaderDepth)
		depthInfinity := depth == "" || depth == "infinity"
		entry, created, err := h.FS.Copy(r.Context(), srcUser, srcSub, dstUser, dstSub, overwrite, depthInfinity)
		if err != nil {
			writeMoveCopyErr(w, err)
			return
		}
		emitMoveCopyHeaders(w, h.InstanceID, entry)
		if created {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}

	entry, created, err := h.FS.Move(r.Context(), srcUser, srcSub, dstUser, dstSub, overwrite)
	if err != nil {
		writeMoveCopyErr(w, err)
		return
	}
	emitMoveCopyHeaders(w, h.InstanceID, entry)
	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func emitMoveCopyHeaders(w http.ResponseWriter, instanceID string, entry *Entry) {
	if entry == nil {
		w.Header().Set("Content-Length", "0")
		return
	}
	w.Header().Set(HeaderOCETag, `"`+entry.ETag+`"`)
	w.Header().Set(HeaderOCFileID, FileID(entry.NumericID, instanceID))
	w.Header().Set("Last-Modified", entry.ModTime.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", "0")
}

func writeMoveCopyErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrExists):
		http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
	default:
		writeFSError(w, err)
	}
}

func (h *Handler) parseDestination(r *http.Request) (user, sub string, err error) {
	raw := r.Header.Get(HeaderDestination)
	if raw == "" {
		return "", "", errors.New("missing Destination")
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", errors.New("invalid Destination")
	}
	p := u.Path
	if p == "" {
		return "", "", errors.New("invalid Destination path")
	}
	if !strings.HasPrefix(p, h.Prefix) {
		return "", "", errors.New("Destination outside DAV namespace")
	}
	user, sub, ok := h.parsePath(p)
	if !ok {
		return "", "", errors.New("invalid Destination path")
	}
	return user, sub, nil
}

func parseOverwrite(r *http.Request) bool {
	v := strings.ToUpper(strings.TrimSpace(r.Header.Get(HeaderOverwrite)))
	if v == "" {
		return true
	}
	return v == "T"
}
