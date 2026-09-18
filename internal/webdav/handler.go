package webdav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

const (
	StatusMultiStatus = 207

	HeaderDepth       = "Depth"
	HeaderDAV         = "DAV"
	HeaderAllow       = "Allow"
	HeaderMSAuthor    = "MS-Author-Via"
	HeaderIfMatch     = "If-Match"
	HeaderIfNoneMatch = "If-None-Match"
	HeaderOCMtime     = "X-OC-Mtime"
	HeaderOCChunked   = "OC-Chunked"
	HeaderOCETag      = "OC-ETag"
	HeaderOCFileID    = "OC-FileId"
	HeaderOCChecksum  = "OC-Checksum"
	HeaderOCTotalLen  = "OC-Total-Length"
	HeaderIf          = "If"

	davCompliance  = "1, 3, extended-mkcol"
	allowedMethods = "OPTIONS, GET, HEAD, PROPFIND, PUT, MKCOL, DELETE, MOVE, COPY"
	contentTypeXML = "application/xml; charset=utf-8"

	HeaderDestination = "Destination"
	HeaderOverwrite   = "Overwrite"
)

type Handler struct {
	Prefix         string
	FS             FS
	InstanceID     string
	OwnerUID       func(*http.Request) string
	OwnerName      func(uid string) string
	Quota          func(ctx context.Context, uid string) (used, available int64, unlimited bool)
	FilesPrefix    string
	Assemble       func(ctx context.Context, srcUser, transferID, destUser, destPath string, overwrite bool, mtime *time.Time, checksum, ifHeader string) (*Entry, bool, error)
	Restore        func(ctx context.Context, srcUser, locationID, destUser, destPath string, overwrite bool) (*Entry, bool, error)
	RestoreVersion func(ctx context.Context, srcUser, fileID, revision, destUser string) (*Entry, bool, error)
}

// ErrInvalidPrefix is returned by NewHandler when the mount prefix does not
// start with a leading slash.
var ErrInvalidPrefix = errors.New("webdav: prefix must start with /")

// NewHandler constructs a Handler mounted at prefix. The prefix is normalized
// to end with a trailing slash.
func NewHandler(prefix string, fs FS, instanceID string) (*Handler, error) {
	if prefix == "" || prefix[0] != '/' {
		return nil, ErrInvalidPrefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Handler{Prefix: prefix, FS: fs, InstanceID: instanceID}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		h.options(w, r)
	case "PROPFIND":
		h.propfind(w, r)
	case http.MethodGet:
		h.get(w, r, true)
	case http.MethodHead:
		h.get(w, r, false)
	case http.MethodPut:
		h.put(w, r)
	case "MKCOL":
		h.mkcol(w, r)
	case http.MethodDelete:
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
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
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

	baseHref := h.hrefPrefix(user) + strings.TrimSuffix(sub, "/")
	if root.IsDir && !strings.HasSuffix(baseHref, "/") {
		baseHref += "/"
	}

	pctx := PropfindContext{
		BaseHref:         baseHref,
		InstanceID:       h.InstanceID,
		OwnerID:          user,
		OwnerDisplayName: h.ownerDisplayName(user),
		EmitQuota:        sub == "/" && root.IsDir,
		QuotaAvailable:   -3,
	}
	if pctx.EmitQuota && h.Quota != nil {
		used, available, unlimited := h.Quota(r.Context(), user)
		pctx.QuotaUsed = used
		if unlimited {
			pctx.QuotaAvailable = -3
		} else {
			pctx.QuotaAvailable = available
		}
	}

	var buf bytes.Buffer
	WriteMultistatus(&buf, pctx, entries)

	w.Header().Set("Content-Type", contentTypeXML)
	w.WriteHeader(StatusMultiStatus)
	_, _ = w.Write(buf.Bytes())
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, writeBody bool) {
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
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
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
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

	body := io.Reader(r.Body)
	defer r.Body.Close()
	if r.ContentLength >= 0 {
		body = io.LimitReader(r.Body, r.ContentLength)
	}

	entry, created, err := h.FS.Write(r.Context(), user, sub, body, mtimePtr)
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

func (h *Handler) authorizePath(w http.ResponseWriter, r *http.Request) (user, sub string, ok bool) {
	if h.OwnerUID != nil && h.OwnerUID(r) == "" {
		writeWebDAVUnauthorized(w)
		return "", "", false
	}
	user, sub, ok = h.requestPath(r)
	if !ok {
		http.NotFound(w, r)
		return "", "", false
	}
	principal, authed := auth.UserFromContext(r.Context())
	if !authed || principal.UID != user {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return "", "", false
	}
	return user, sub, true
}

func (h *Handler) requestPath(r *http.Request) (user, sub string, ok bool) {
	if h.OwnerUID != nil {
		return h.parseOwnerPath(r.URL.Path, h.OwnerUID(r))
	}
	return h.parsePath(r.URL.Path)
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

func (h *Handler) hrefPrefix(user string) string {
	if h.OwnerUID != nil {
		return h.Prefix
	}
	return h.Prefix + user
}

func (h *Handler) ownerDisplayName(uid string) string {
	if h.OwnerName != nil {
		if name := h.OwnerName(uid); name != "" {
			return name
		}
	}
	return uid
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
	case errors.Is(err, ErrPrecondition):
		http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
	case errors.Is(err, ErrBadRequest):
		http.Error(w, "Bad Request", http.StatusBadRequest)
	case errors.Is(err, ErrExists):
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	case errors.Is(err, ErrNotDir), errors.Is(err, ErrIsDir), errors.Is(err, ErrParentMissing):
		http.Error(w, "Conflict", http.StatusConflict)
	default:
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *Handler) mkcol(w http.ResponseWriter, r *http.Request) {
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
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
	var entry *Entry
	var err error
	if mm, ok := h.FS.(MetaMkdirFS); ok {
		meta := CollectionMeta{}
		if raw := r.Header.Get(HeaderDestination); raw != "" {
			if _, dest, derr := h.parseFilesDestination(raw); derr == nil {
				meta.Destination = dest
			}
		}
		if v := r.Header.Get(HeaderOCTotalLen); v != "" {
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr == nil {
				meta.TotalLength = n
			}
		}
		entry, err = mm.MkdirMeta(r.Context(), user, sub, meta)
	} else {
		entry, err = h.FS.Mkdir(r.Context(), user, sub)
	}
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
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
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
	srcUser, srcSub, ok := h.authorizePath(w, r)
	if !ok {
		return
	}
	if srcSub == "/" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	overwrite := parseOverwrite(r)

	if !isCopy && h.RestoreVersion != nil {
		if fileID, rev, ok := parseVersionSource(srcSub); ok {
			dstUser, derr := h.parseVersionRestoreDestination(r)
			if derr != nil {
				http.Error(w, derr.Error(), http.StatusBadRequest)
				return
			}
			if dstUser != srcUser {
				http.Error(w, "Bad Gateway", http.StatusBadGateway)
				return
			}
			entry, created, err := h.RestoreVersion(r.Context(), srcUser, fileID, rev, dstUser)
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
	}

	if !isCopy && h.Restore != nil {
		if loc, ok := restoreLocationID(srcSub); ok {
			dstUser, dstSub, useOriginal, derr := h.parseRestoreDestination(r)
			if derr != nil {
				http.Error(w, derr.Error(), http.StatusBadRequest)
				return
			}
			if dstUser != srcUser {
				http.Error(w, "Bad Gateway", http.StatusBadGateway)
				return
			}
			destPath := dstSub
			if useOriginal {
				destPath = ""
			}
			entry, created, err := h.Restore(r.Context(), srcUser, loc, dstUser, destPath, overwrite)
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
	}

	if !isCopy && h.Assemble != nil {
		if tid, ok := assembleTransferID(srcSub); ok {
			dstUser, dstSub, derr := h.parseFilesDestination(r.Header.Get(HeaderDestination))
			if derr != nil {
				http.Error(w, derr.Error(), http.StatusBadRequest)
				return
			}
			if dstUser != srcUser {
				http.Error(w, "Bad Gateway", http.StatusBadGateway)
				return
			}
			var mtimePtr *time.Time
			if v := r.Header.Get(HeaderOCMtime); v != "" {
				secs, err := strconv.ParseInt(v, 10, 64)
				if err == nil {
					t := time.Unix(secs, 0).UTC()
					mtimePtr = &t
				}
			}
			entry, created, err := h.Assemble(r.Context(), srcUser, tid, dstUser, dstSub, overwrite, mtimePtr, r.Header.Get(HeaderOCChecksum), r.Header.Get(HeaderIf))
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
	case errors.Is(err, ErrExists), errors.Is(err, ErrPrecondition):
		http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
	default:
		writeFSError(w, err)
	}
}

func (h *Handler) filesPrefix() string {
	p := h.FilesPrefix
	if p == "" {
		p = "/remote.php/dav/files/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func assembleTransferID(sub string) (string, bool) {
	np := normalizePath(sub)
	if path.Base(np) != ".file" {
		return "", false
	}
	tid := strings.TrimPrefix(path.Dir(np), "/")
	if tid == "" || strings.Contains(tid, "/") {
		return "", false
	}
	return tid, true
}

func parseVersionSource(sub string) (fileID, rev string, ok bool) {
	np := normalizePath(sub)
	rel := strings.TrimPrefix(np, "/")
	coll, rest, found := strings.Cut(rel, "/")
	if !found || coll != "versions" {
		return "", "", false
	}
	fileID, rev, found = strings.Cut(rest, "/")
	if !found || fileID == "" || rev == "" || strings.Contains(rev, "/") {
		return "", "", false
	}
	return fileID, rev, true
}

func (h *Handler) parseVersionRestoreDestination(r *http.Request) (user string, err error) {
	raw := r.Header.Get(HeaderDestination)
	if raw == "" {
		return "", errors.New("missing Destination")
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", errors.New("invalid Destination")
	}
	p := u.Path
	if p == "" {
		return "", errors.New("invalid Destination path")
	}
	if !strings.HasPrefix(p, h.Prefix) {
		return "", errors.New("destination outside DAV namespace")
	}
	user, sub, ok := h.parsePath(p)
	if !ok {
		return "", errors.New("invalid Destination path")
	}
	np := normalizePath(sub)
	if np == "/restore" || strings.HasPrefix(np+"/", "/restore/") {
		return user, nil
	}
	return "", errors.New("destination outside restore namespace")
}

func restoreLocationID(sub string) (string, bool) {
	np := normalizePath(sub)
	rel := strings.TrimPrefix(np, "/")
	coll, loc, ok := strings.Cut(rel, "/")
	if !ok || loc == "" || strings.Contains(loc, "/") {
		return "", false
	}
	if coll != "trash" && coll != "restore" {
		return "", false
	}
	return loc, true
}

func (h *Handler) parseRestoreDestination(r *http.Request) (user, destPath string, useOriginal bool, err error) {
	raw := r.Header.Get(HeaderDestination)
	if raw == "" {
		return "", "", false, errors.New("missing Destination")
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", false, errors.New("invalid Destination")
	}
	p := u.Path
	if p == "" {
		return "", "", false, errors.New("invalid Destination path")
	}
	if strings.HasPrefix(p, h.filesPrefix()) {
		user, destPath, err = h.parseFilesDestination(raw)
		return user, destPath, false, err
	}
	if strings.HasPrefix(p, h.Prefix) {
		user, sub, ok := h.parsePath(p)
		if !ok {
			return "", "", false, errors.New("invalid Destination path")
		}
		np := normalizePath(sub)
		if np == "/restore" || strings.HasPrefix(np+"/", "/restore/") {
			return user, "", true, nil
		}
		return "", "", false, errors.New("destination outside restore namespace")
	}
	return "", "", false, errors.New("destination outside DAV namespace")
}

func (h *Handler) parseFilesDestination(raw string) (user, sub string, err error) {
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
	prefix := h.filesPrefix()
	if !strings.HasPrefix(p, prefix) {
		return "", "", errors.New("destination outside DAV namespace")
	}
	rest := strings.TrimPrefix(p, prefix)
	if rest == "" {
		return "", "", errors.New("invalid Destination path")
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return rest, "/", nil
	}
	user = rest[:slash]
	sub = rest[slash:]
	if user == "" {
		return "", "", errors.New("invalid Destination path")
	}
	if sub == "" {
		sub = "/"
	}
	return user, sub, nil
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
		return "", "", errors.New("destination outside DAV namespace")
	}
	var ok bool
	if h.OwnerUID != nil {
		user, sub, ok = h.parseOwnerPath(p, h.OwnerUID(r))
	} else {
		user, sub, ok = h.parsePath(p)
	}
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
