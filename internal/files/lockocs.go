package files

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const (
	ocsLockPrefixV1 = "/ocs/v1.php/apps/files_lock"
	ocsLockPrefixV2 = "/ocs/v2.php/apps/files_lock"
)

// LockHandler serves OCS files_lock PUT/DELETE by numeric fileid.
type LockHandler struct {
	DAV     *DAV
	Version ocs.Version
}

func (h LockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeLockOCS(w, r, h.Version, ocs.RespondUnauthorised, "Current user is not logged in", nil)
		return
	}
	id, ok := parseLockFileID(r.URL.Path)
	if !ok {
		writeLockOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
		return
	}
	switch r.Method {
	case http.MethodPut:
		h.lock(w, r, p.UID, id)
	case http.MethodDelete:
		h.unlock(w, r, p.UID, id)
	default:
		writeLockOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
	}
}

func (h LockHandler) lock(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	if h.DAV == nil {
		writeLockOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	info, err := h.DAV.LockByFileID(r.Context(), uid, id, webdav.LockRequest{Owner: uid})
	if err != nil {
		writeLockOCSErr(w, r, h.Version, err)
		return
	}
	writeLockOCS(w, r, h.Version, 0, "", ocs.Obj(
		ocs.K("id", id),
		ocs.K("lock", true),
		ocs.K("token", info.Token),
		ocs.K("owner", info.Owner),
	))
}

func (h LockHandler) unlock(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	if h.DAV == nil {
		writeLockOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	token := strings.TrimSpace(r.Header.Get(webdav.HeaderLockToken))
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if err := h.DAV.UnlockByFileID(r.Context(), uid, id, token); err != nil {
		writeLockOCSErr(w, r, h.Version, err)
		return
	}
	writeLockOCS(w, r, h.Version, 0, "", []any{})
}

func parseLockFileID(path string) (int64, bool) {
	rest, ok := lockPathRemainder(path)
	if !ok {
		return 0, false
	}
	rest = strings.Trim(rest, "/")
	rest = strings.TrimPrefix(rest, "lock/")
	rest = strings.Trim(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func lockPathRemainder(path string) (string, bool) {
	switch {
	case strings.HasPrefix(path, ocsLockPrefixV1):
		return strings.TrimPrefix(path, ocsLockPrefixV1), true
	case strings.HasPrefix(path, ocsLockPrefixV2):
		return strings.TrimPrefix(path, ocsLockPrefixV2), true
	default:
		return "", false
	}
}

func writeLockOCSErr(w http.ResponseWriter, r *http.Request, version ocs.Version, err error) {
	switch {
	case errors.Is(err, webdav.ErrNotFound), errors.Is(err, ErrNotFound):
		writeLockOCS(w, r, version, ocs.RespondNotFound, "File not found", nil)
	case errors.Is(err, webdav.ErrLocked):
		writeLockOCS(w, r, version, 423, "File is locked", nil)
	case errors.Is(err, webdav.ErrConflict), errors.Is(err, webdav.ErrBadRequest):
		writeLockOCS(w, r, version, 400, "Invalid lock token", nil)
	default:
		writeLockOCS(w, r, version, ocs.RespondServerError, "Internal Server Error", nil)
	}
}

func writeLockOCS(w http.ResponseWriter, r *http.Request, version ocs.Version, code int, message string, data any) {
	format := ocs.NegotiateFormat(r.URL.Query().Get("format"), r.Header.Get("Accept"))
	if code == 0 {
		if version == ocs.V1 {
			code = ocs.StatusOKv1
		} else {
			code = ocs.StatusOKv2
		}
	}
	meta := ocs.Meta{StatusCode: code, Message: message}
	if code != ocs.StatusOKv1 && code != ocs.StatusOKv2 {
		meta.Status = "failure"
	}
	body, contentType, err := ocs.Render(version, format, meta, data)
	if err != nil {
		http.Error(w, "render", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(ocs.Map(version, code))
	_, _ = w.Write(body)
}
