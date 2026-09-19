package ocm

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

const (
	ocsRemotePrefixV1 = "/ocs/v1.php/apps/files_sharing/api/v1/remote_shares"
	ocsRemotePrefixV2 = "/ocs/v2.php/apps/files_sharing/api/v1/remote_shares"
)

// RemoteSharesHandler serves OCS remote_shares list/get/delete.
type RemoteSharesHandler struct {
	Store   Store
	Users   users.Store
	Version ocs.Version
}

func (h RemoteSharesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeOCS(w, r, h.Version, ocs.RespondUnauthorised, "Current user is not logged in", nil)
		return
	}
	u, err := h.Users.GetByUID(r.Context(), p.UID)
	if err != nil {
		writeOCS(w, r, h.Version, ocs.RespondUnauthorised, "Current user is not logged in", nil)
		return
	}
	id, collection, ok := parseRemotePath(r.URL.Path, h.prefix())
	if !ok {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
		return
	}
	switch {
	case collection && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.list(w, r, u.ID)
	case !collection && r.Method == http.MethodGet:
		h.get(w, r, u.ID, id)
	case !collection && r.Method == http.MethodDelete:
		h.delete(w, r, u.ID, id)
	default:
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
	}
}

func (h RemoteSharesHandler) prefix() string {
	if h.Version == ocs.V1 {
		return ocsRemotePrefixV1
	}
	return ocsRemotePrefixV2
}

func (h RemoteSharesHandler) list(w http.ResponseWriter, r *http.Request, userID int64) {
	items, err := h.Store.List(r.Context(), userID)
	if err != nil {
		writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	data := make([]any, 0, len(items))
	for i := range items {
		data = append(data, remoteSharePayload(&items[i]))
	}
	writeOCS(w, r, h.Version, 0, "", data)
}

func (h RemoteSharesHandler) get(w http.ResponseWriter, r *http.Request, userID, id int64) {
	in, err := h.Store.Get(r.Context(), userID, id)
	if err != nil {
		writeRemoteErr(w, r, h.Version, err)
		return
	}
	writeOCS(w, r, h.Version, 0, "", remoteSharePayload(in))
}

func (h RemoteSharesHandler) delete(w http.ResponseWriter, r *http.Request, userID, id int64) {
	if err := h.Store.Delete(r.Context(), userID, id); err != nil {
		writeRemoteErr(w, r, h.Version, err)
		return
	}
	writeOCS(w, r, h.Version, 0, "", []any{})
}

func remoteSharePayload(in *Incoming) any {
	accepted := in.Accepted != 0
	return ocs.Obj(
		ocs.K("id", in.ID),
		ocs.K("remote", in.Remote),
		ocs.K("remote_id", in.RemoteID),
		ocs.K("share_token", in.Token),
		ocs.K("name", in.Name),
		ocs.K("owner", in.Owner),
		ocs.K("user", in.UserUID),
		ocs.K("mountpoint", "/"+in.Name),
		ocs.K("accepted", accepted),
		ocs.K("permissions", in.Permissions),
	)
}

func parseRemotePath(path, prefix string) (id int64, collection, ok bool) {
	if !strings.HasPrefix(path, prefix) {
		return 0, false, false
	}
	rest := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if rest == "" {
		return 0, true, true
	}
	if strings.Contains(rest, "/") {
		return 0, false, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n <= 0 {
		return 0, false, false
	}
	return n, false, true
}

func writeRemoteErr(w http.ResponseWriter, r *http.Request, version ocs.Version, err error) {
	if errors.Is(err, ErrNotFound) {
		writeOCS(w, r, version, ocs.RespondNotFound, "Not found", nil)
		return
	}
	writeOCS(w, r, version, ocs.RespondServerError, "Internal Server Error", nil)
}

func writeOCS(w http.ResponseWriter, r *http.Request, version ocs.Version, code int, message string, data any) {
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
