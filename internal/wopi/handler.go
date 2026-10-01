package wopi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// WopiFilesPrefix is the upstream richdocuments WOPI callback path the
// FilesHandler is mounted under (HandlePrefix strips nothing, so the
// handler parses the tail itself).
const WopiFilesPrefix = "/index.php/apps/richdocuments/wopi/files/"

// MintHandler serves GET /index.php/apps/richdocuments/wopi/token?fileId=N,
// mounted WITH session auth (webdav.Auth): the session ctx carries an
// enrolled user's unlocked key, which is what makes minting work for them
// while anonymous callbacks hit the documented 403 boundary.
type MintHandler struct {
	Svc *Service
}

type mintResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	WopiSrc   string `json:"wopi_src"`
	CanWrite  bool   `json:"can_write"`
}

func (h *MintHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw := r.URL.Query().Get("fileId")
	fileID, err := strconv.ParseInt(raw, 10, 64)
	if raw == "" || err != nil || fileID <= 0 {
		http.Error(w, "bad fileId", http.StatusBadRequest)
		return
	}
	tok, expiresAt, canWrite, err := h.Svc.Mint(r.Context(), p.UID, fileID)
	if err != nil {
		if errors.Is(err, errIsDir) {
			// Mint rejects directories as a client error (the anonymous
			// callbacks report the same condition as 404).
			http.Error(w, "file id is a directory", http.StatusBadRequest)
			return
		}
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mintResponse{
		Token:     tok,
		ExpiresAt: expiresAt.UnixMilli(),
		WopiSrc:   absoluteBase(r) + WopiFilesPrefix + strconv.FormatInt(fileID, 10),
		CanWrite:  canWrite,
	})
}

// FilesHandler serves the Collabora-facing WOPI callbacks under
// WopiFilesPrefix, mounted WITHOUT session middleware: the access_token
// query parameter is the only credential (a bearer token for exactly one
// file id), validated BEFORE any file resolution so a bad token never
// learns whether a file exists.
type FilesHandler struct {
	Svc *Service
}

type fileInfo struct {
	BaseFileName     string `json:"BaseFileName"`
	Size             int64  `json:"Size"`
	OwnerID          string `json:"OwnerId"`
	UserID           string `json:"UserId"`
	UserFriendlyName string `json:"UserFriendlyName"`
	UserCanWrite     bool   `json:"UserCanWrite"`
	Version          string `json:"Version"`
	LastModifiedTime string `json:"LastModifiedTime"`
}

func (h *FilesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, contents, ok := parseFileTail(strings.TrimPrefix(r.URL.Path, WopiFilesPrefix))
	if !ok {
		http.NotFound(w, r)
		return
	}
	tok, err := h.Svc.Authenticate(r.Context(), r.URL.Query().Get("access_token"))
	if err != nil || tok.FileID != id {
		// Invalid, expired, missing, or minted for a different file id: one
		// undifferentiated 401 — the token is validated before the file is
		// resolved, so nothing about the file leaks.
		http.Error(w, "invalid access token", http.StatusUnauthorized)
		return
	}
	fsUser, fsPath, perms, err := h.Svc.resolve(r.Context(), tok.UID, id)
	if err != nil {
		mapError(w, err)
		return
	}
	canWrite := tok.CanWrite && perms&webdav.PermUpdate != 0
	switch {
	case r.Method == http.MethodGet && !contents:
		h.checkFileInfo(w, r, tok, id, fsUser, fsPath, canWrite)
	case r.Method == http.MethodGet && contents:
		h.getFile(w, r, fsUser, fsPath)
	case r.Method == http.MethodPost && contents:
		h.putFile(w, r, fsUser, fsPath, canWrite)
	case r.Method == http.MethodPost:
		h.postOp(w, r, fsUser, fsPath, canWrite)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func parseFileTail(tail string) (id int64, contents, ok bool) {
	idStr, isContents := strings.CutSuffix(tail, "/contents")
	if idStr == "" || strings.Contains(idStr, "/") {
		return 0, false, false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return 0, false, false
	}
	return id, isContents, true
}

// checkFileInfo is the WOPI CheckFileInfo operation (minimal field set).
func (h *FilesHandler) checkFileInfo(w http.ResponseWriter, r *http.Request, tok *Token, id int64, fsUser, fsPath string, canWrite bool) {
	entry, err := h.Svc.Files.Stat(r.Context(), fsUser, fsPath)
	if err != nil {
		mapError(w, err)
		return
	}
	owner, err := h.Svc.ownerUID(r.Context(), id)
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileInfo{
		BaseFileName:     path.Base(fsPath),
		Size:             entry.Size,
		OwnerID:          owner,
		UserID:           tok.UID,
		UserFriendlyName: h.Svc.friendlyName(r.Context(), tok.UID),
		UserCanWrite:     canWrite,
		Version:          entry.ETag,
		LastModifiedTime: entry.ModTime.UTC().Format(time.RFC3339),
	})
}

// getFile is the WOPI GetFile operation.
func (h *FilesHandler) getFile(w http.ResponseWriter, r *http.Request, fsUser, fsPath string) {
	rc, _, err := h.Svc.Files.Read(r.Context(), fsUser, fsPath)
	if err != nil {
		mapError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// putFile is the WOPI PutFile operation: write access plus the X-WOPI-Lock
// handshake — a locked file accepts the write only with the current lock id.
func (h *FilesHandler) putFile(w http.ResponseWriter, r *http.Request, fsUser, fsPath string, canWrite bool) {
	if !canWrite {
		http.Error(w, "read only", http.StatusForbidden)
		return
	}
	current, locked, err := h.Svc.Files.LockTokenAt(r.Context(), fsUser, fsPath)
	if err != nil {
		mapError(w, err)
		return
	}
	if locked && r.Header.Get("X-WOPI-Lock") != current {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "locked"})
		return
	}
	entry, _, err := h.Svc.Files.Write(r.Context(), fsUser, fsPath, r.Body, nil)
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"LastModifiedTime": entry.ModTime.UTC().Format(time.RFC3339),
	})
}

// postOp dispatches the X-WOPI-Override lock operations on POST {id}.
func (h *FilesHandler) postOp(w http.ResponseWriter, r *http.Request, fsUser, fsPath string, canWrite bool) {
	lockID := r.Header.Get("X-WOPI-Lock")
	switch r.Header.Get("X-WOPI-Override") {
	case "LOCK":
		if lockID == "" {
			http.Error(w, "missing X-WOPI-Lock", http.StatusBadRequest)
			return
		}
		if !canWrite {
			http.Error(w, "read only", http.StatusForbidden)
			return
		}
		// Fresh lock, idempotent re-LOCK with the same id, or ErrLocked
		// (409) when another client holds it.
		if _, err := h.Svc.Files.LockWithToken(r.Context(), fsUser, fsPath, lockID, 0); err != nil {
			mapError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "UNLOCK":
		if lockID == "" {
			http.Error(w, "missing X-WOPI-Lock", http.StatusBadRequest)
			return
		}
		if !canWrite {
			// Same rule as LOCK: a read-only token must not release another
			// editor's lock (lock ids are bearer-shaped, not capabilities).
			http.Error(w, "read only", http.StatusForbidden)
			return
		}
		if err := h.Svc.Files.UnlockWithToken(r.Context(), fsUser, fsPath, lockID); err != nil {
			mapError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "REFRESH_LOCK":
		if lockID == "" {
			http.Error(w, "missing X-WOPI-Lock", http.StatusBadRequest)
			return
		}
		if !canWrite {
			http.Error(w, "read only", http.StatusForbidden)
			return
		}
		current, locked, err := h.Svc.Files.LockTokenAt(r.Context(), fsUser, fsPath)
		if err != nil {
			mapError(w, err)
			return
		}
		if !locked || current != lockID {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "locked"})
			return
		}
		if _, err := h.Svc.Files.LockWithToken(r.Context(), fsUser, fsPath, lockID, 0); err != nil {
			mapError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "unknown X-WOPI-Override", http.StatusNotImplemented)
	}
}

func mapError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound), errors.Is(err, errIsDir), errors.Is(err, webdav.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrTokenNotFound):
		http.Error(w, "invalid access token", http.StatusUnauthorized)
	case keyLocked(err):
		http.Error(w, "encrypted: key locked", http.StatusForbidden)
	case errors.Is(err, webdav.ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, webdav.ErrLocked), errors.Is(err, webdav.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "locked"})
	case errors.Is(err, webdav.ErrNotImplemented):
		http.Error(w, "not implemented", http.StatusNotImplemented)
	case errors.Is(err, webdav.ErrBadRequest):
		http.Error(w, "bad request", http.StatusBadRequest)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// absoluteBase mirrors web/login_v2.go's defaultBaseURL (the nearest
// absolute-URL precedent): the request scheme with the trusted-proxy
// X-Forwarded-Proto / X-Forwarded-Host overrides.
func absoluteBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}
