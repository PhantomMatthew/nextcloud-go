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
// enrolled user's unlocked key, which is what makes minting work for them —
// and, with Keys wired (ADR-0107), seals that key under the new token so the
// anonymous callbacks open it instead of hitting the 403 boundary.
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
// learns whether a file exists. With Keys wired, a token carrying an
// ADR-0107 key wrap also unlocks the minter's key into the request ctx.
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
	if h.Svc.Keys != nil {
		// Token-bound key wrap (ADR-0107): a token minted by an enrolled
		// user opens its wrap here, so the callback resolves the file
		// through the minter's own unlocked key instead of hitting the
		// anonymous ErrKeyLocked → 403 boundary. No wrap row (nil priv)
		// keeps the anonymous ctx — pre-0026 and keylessly-minted tokens
		// keep the documented boundary. A wrap error maps through mapError
		// (ErrIntegrity → 500, fail-closed): a corrupt wrap must NEVER
		// degrade to a silent keyless 403.
		priv, err := h.Svc.Keys.UnlockForWOPIToken(r.Context(), tok.Token, tok.FileID)
		if err != nil {
			mapError(w, err)
			return
		}
		if len(priv) > 0 {
			// Best-effort zeroing once the request completes, mirroring the
			// auth middleware's key hygiene (middleware.go).
			defer clear(priv)
			r = r.WithContext(auth.WithUser(r.Context(), &auth.Principal{UID: tok.UID, Enabled: true, UnlockedKey: priv}))
		}
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
		if strings.EqualFold(r.Header.Get("X-WOPI-Override"), "PUT_RELATIVE") {
			h.putRelativeFile(w, r, fsUser, fsPath, canWrite)
		} else {
			h.putFile(w, r, fsUser, fsPath, canWrite)
		}
	case r.Method == http.MethodPost:
		h.postOp(w, r, id, fsUser, fsPath, canWrite)
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

// putRelativeFile is the WOPI PutRelativeFile operation (POST {id}/contents
// with X-WOPI-Override: PUT_RELATIVE): the body lands as a NEW file next to
// the current one. X-WOPI-RelativeTarget names it exactly (409 on conflict
// unless X-WOPI-OverwriteRelativeTarget: true); X-WOPI-SuggestedTarget gets
// deduplicated with a numeric suffix (a bare ".ext" suggestion keeps the
// current file's base name). Exactly one of the two headers is required.
func (h *FilesHandler) putRelativeFile(w http.ResponseWriter, r *http.Request, fsUser, fsPath string, canWrite bool) {
	if !canWrite {
		http.Error(w, "read only", http.StatusForbidden)
		return
	}
	rel := r.Header.Get("X-WOPI-RelativeTarget")
	suggested := r.Header.Get("X-WOPI-SuggestedTarget")
	if (rel == "") == (suggested == "") {
		http.Error(w, "exactly one of X-WOPI-RelativeTarget and X-WOPI-SuggestedTarget is required", http.StatusBadRequest)
		return
	}
	overwrite := strings.EqualFold(r.Header.Get("X-WOPI-OverwriteRelativeTarget"), "true")
	name, err := relativeTargetName(fsPath, rel, suggested)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	parent := path.Dir(fsPath)
	// A suggested target dedupes against existing siblings.
	if suggested != "" {
		name = dedupeSiblingName(r, h.Svc, fsUser, parent, name)
	}
	newPath := joinSibling(parent, name)
	if !overwrite {
		if _, err := h.Svc.Files.Stat(r.Context(), fsUser, newPath); err == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "file exists"})
			return
		}
	}
	if _, _, err := h.Svc.Files.Write(r.Context(), fsUser, newPath, r.Body, nil); err != nil {
		mapError(w, err)
		return
	}
	entry, err := h.Svc.Files.Stat(r.Context(), fsUser, newPath)
	if err != nil {
		mapError(w, err)
		return
	}
	//nolint:gosec // G115: filecache IDs are positive serial keys
	h.writeNewFileURLs(w, r, name, int64(entry.NumericID))
}

// renameFile is the WOPI RenameFile operation (POST {id} with
// X-WOPI-Override: RENAME_FILE): the file moves to X-WOPI-RequestedName
// within its directory, keeping its filecache id — so the caller's token
// stays valid across the rename. A locked file renames only with the
// current lock id.
func (h *FilesHandler) renameFile(w http.ResponseWriter, r *http.Request, id int64, fsUser, fsPath string, canWrite bool) {
	if !canWrite {
		http.Error(w, "read only", http.StatusForbidden)
		return
	}
	name, err := sanitizeWOPIName(r.Header.Get("X-WOPI-RequestedName"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	newPath := joinSibling(path.Dir(fsPath), name)
	if _, _, err := h.Svc.Files.Move(r.Context(), fsUser, fsPath, fsUser, newPath, false); err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"Name": name,
		"Url":  absoluteBase(r) + WopiFilesPrefix + strconv.FormatInt(id, 10),
	})
}

// writeNewFileURLs answers PutRelativeFile with the new file's WOPI and
// viewer URLs (host URLs share the viewer page for both view and edit).
func (h *FilesHandler) writeNewFileURLs(w http.ResponseWriter, r *http.Request, name string, newID int64) {
	wopiURL := absoluteBase(r) + WopiFilesPrefix + strconv.FormatInt(newID, 10)
	viewerURL := absoluteBase(r) + "/index.php/apps/richdocuments/index?fileId=" + strconv.FormatInt(newID, 10)
	writeJSON(w, http.StatusOK, map[string]string{
		"Name":        name,
		"Url":         wopiURL,
		"HostViewUrl": viewerURL,
		"HostEditUrl": viewerURL,
	})
}

// relativeTargetName resolves the new file's name from the two target
// headers; a suggested ".ext" keeps the current base name.
func relativeTargetName(fsPath, rel, suggested string) (string, error) {
	if rel != "" {
		return sanitizeWOPIName(rel)
	}
	if strings.HasPrefix(suggested, ".") {
		base := strings.TrimSuffix(path.Base(fsPath), path.Ext(fsPath))
		return sanitizeWOPIName(base + suggested)
	}
	return sanitizeWOPIName(suggested)
}

// sanitizeWOPIName rejects names that would escape the file's directory or
// name nothing.
func sanitizeWOPIName(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return "", errors.New("invalid target file name")
	}
	return name, nil
}

// dedupeSiblingName appends " N" before the extension until the name is
// free (bounded; the final candidate's write still races safely through
// the overwrite=false stat gate the caller applies).
func dedupeSiblingName(r *http.Request, svc *Service, fsUser, parent, name string) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	candidate := name
	for n := 1; n < 100; n++ {
		if _, err := svc.Files.Stat(r.Context(), fsUser, joinSibling(parent, candidate)); err != nil {
			return candidate
		}
		candidate = base + " " + strconv.Itoa(n) + ext
	}
	return candidate
}

func joinSibling(parent, name string) string {
	if parent == "/" || parent == "." {
		return "/" + name
	}
	return parent + "/" + name
}

// postOp dispatches the X-WOPI-Override operations on POST {id}: the lock
// verbs and RENAME_FILE (PutRelativeFile rides POST {id}/contents instead).
func (h *FilesHandler) postOp(w http.ResponseWriter, r *http.Request, id int64, fsUser, fsPath string, canWrite bool) {
	lockID := r.Header.Get("X-WOPI-Lock")
	switch r.Header.Get("X-WOPI-Override") {
	case "RENAME_FILE":
		h.renameFile(w, r, id, fsUser, fsPath, canWrite)
		return
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
