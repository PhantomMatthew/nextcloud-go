package sharing

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const (
	ocsSharesPrefixV1 = "/ocs/v1.php/apps/files_sharing/api/v1/shares"
	ocsSharesPrefixV2 = "/ocs/v2.php/apps/files_sharing/api/v1/shares"
)

// Handler serves OCS files_sharing shareType=3 CRUD.
type Handler struct {
	Service *Service
	Version ocs.Version
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeOCS(w, r, h.Version, ocs.RespondUnauthorised, "Current user is not logged in", nil)
		return
	}
	id, ok := parseShareID(r.URL.Path)
	if !ok {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
		return
	}
	switch {
	case id == 0 && r.Method == http.MethodGet:
		h.list(w, r, p.UID)
	case id == 0 && r.Method == http.MethodPost:
		h.create(w, r, p.UID)
	case id > 0 && r.Method == http.MethodGet:
		h.get(w, r, p.UID, id)
	case id > 0 && r.Method == http.MethodPut:
		h.update(w, r, p.UID, id)
	case id > 0 && r.Method == http.MethodDelete:
		h.delete(w, r, p.UID, id)
	default:
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
	}
}

func (h Handler) list(w http.ResponseWriter, r *http.Request, uid string) {
	items, err := h.Service.ListForOwner(r.Context(), uid, r.URL.Query().Get("path"))
	if err != nil {
		writeShareErr(w, r, h.Version, err)
		return
	}
	data := make([]any, 0, len(items))
	for i := range items {
		payload, err := h.Service.SharePayload(r.Context(), r, &items[i])
		if err != nil {
			writeShareErr(w, r, h.Version, err)
			return
		}
		data = append(data, payload)
	}
	writeOCS(w, r, h.Version, 0, "", data)
}

func (h Handler) create(w http.ResponseWriter, r *http.Request, uid string) {
	form, err := readShareForm(r)
	if err != nil {
		writeOCS(w, r, h.Version, 400, "Invalid request", nil)
		return
	}
	path := form.Get("path")
	if path == "" {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Wrong path, file/folder doesn't exist", nil)
		return
	}
	shareType := 0
	if raw := form.Get("shareType"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeOCS(w, r, h.Version, 400, "unknown share type", nil)
			return
		}
		shareType = n
	}
	perms := 0
	if raw := form.Get("permissions"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeOCS(w, r, h.Version, 400, "Invalid permissions", nil)
			return
		}
		perms = n
	}
	sh, err := h.Service.Create(r.Context(), uid, path, shareType, perms, form.Get("password"), form.Get("expireDate"), form.Get("label"))
	if err != nil {
		writeShareErr(w, r, h.Version, err)
		return
	}
	payload, err := h.Service.SharePayload(r.Context(), r, sh)
	if err != nil {
		writeShareErr(w, r, h.Version, err)
		return
	}
	writeOCS(w, r, h.Version, 0, "", payload)
}

func (h Handler) get(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	sh, err := h.Service.GetForOwner(r.Context(), uid, id)
	if err != nil {
		writeShareErr(w, r, h.Version, err)
		return
	}
	payload, err := h.Service.SharePayload(r.Context(), r, sh)
	if err != nil {
		writeShareErr(w, r, h.Version, err)
		return
	}
	writeOCS(w, r, h.Version, 0, "", payload)
}

func (h Handler) update(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	form, err := readShareForm(r)
	if err != nil {
		writeOCS(w, r, h.Version, 400, "Invalid request", nil)
		return
	}
	var perms *int
	if raw, ok := form["permissions"]; ok && len(raw) > 0 {
		n, err := strconv.Atoi(raw[0])
		if err != nil {
			writeOCS(w, r, h.Version, 400, "Invalid permissions", nil)
			return
		}
		perms = &n
	}
	var password *string
	if raw, ok := form["password"]; ok && len(raw) > 0 {
		password = &raw[0]
	}
	var expire *string
	if raw, ok := form["expireDate"]; ok && len(raw) > 0 {
		expire = &raw[0]
	}
	var label *string
	if raw, ok := form["label"]; ok && len(raw) > 0 {
		label = &raw[0]
	}
	sh, err := h.Service.Update(r.Context(), uid, id, perms, password, expire, label)
	if err != nil {
		writeShareErr(w, r, h.Version, err)
		return
	}
	payload, err := h.Service.SharePayload(r.Context(), r, sh)
	if err != nil {
		writeShareErr(w, r, h.Version, err)
		return
	}
	writeOCS(w, r, h.Version, 0, "", payload)
}

func (h Handler) delete(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	if err := h.Service.Delete(r.Context(), uid, id); err != nil {
		writeShareErr(w, r, h.Version, err)
		return
	}
	writeOCS(w, r, h.Version, 0, "", []any{})
}

func readShareForm(r *http.Request) (url.Values, error) {
	vals := url.Values{}
	if r.URL != nil {
		for k, vs := range r.URL.Query() {
			vals[k] = vs
		}
	}
	if r.Body == nil {
		return vals, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	parsed, err := url.ParseQuery(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, err
	}
	for k, vs := range parsed {
		vals[k] = vs
	}
	return vals, nil
}

func parseShareID(path string) (int64, bool) {
	rest, ok := sharePathRemainder(path)
	if !ok {
		return 0, false
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return 0, true
	}
	if strings.Contains(rest, "/") {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func sharePathRemainder(path string) (string, bool) {
	switch {
	case strings.HasPrefix(path, ocsSharesPrefixV1):
		return strings.TrimPrefix(path, ocsSharesPrefixV1), true
	case strings.HasPrefix(path, ocsSharesPrefixV2):
		return strings.TrimPrefix(path, ocsSharesPrefixV2), true
	default:
		return "", false
	}
}

func writeShareErr(w http.ResponseWriter, r *http.Request, version ocs.Version, err error) {
	switch {
	case errors.Is(err, errBadShareType):
		writeOCS(w, r, version, 400, "unknown share type", nil)
	case errors.Is(err, errBadPermissions), errors.Is(err, errBadExpire):
		writeOCS(w, r, version, 400, err.Error(), nil)
	case errors.Is(err, files.ErrNotFound), errors.Is(err, webdav.ErrNotFound):
		writeOCS(w, r, version, ocs.RespondNotFound, "Wrong path, file/folder doesn't exist", nil)
	case errors.Is(err, errUnauthorized):
		writeOCS(w, r, version, ocs.RespondUnauthorised, "Unauthorized", nil)
	default:
		writeOCS(w, r, version, ocs.RespondServerError, "Internal Server Error", nil)
	}
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

func shareMap(sh *files.Share, uid, display, mime string, fileID int64, url, expiration string) any {
	return ocs.Obj(
		ocs.K("id", strconv.FormatInt(sh.ID, 10)),
		ocs.K("share_type", sh.ShareType),
		ocs.K("uid_owner", uid),
		ocs.K("displayname_owner", display),
		ocs.K("permissions", sh.Permissions),
		ocs.K("stime", sh.StimeMs/1000),
		ocs.K("expiration", expiration),
		ocs.K("token", sh.Token),
		ocs.K("path", sh.Path),
		ocs.K("item_type", sh.ItemType),
		ocs.K("mimetype", mime),
		ocs.K("item_source", fileID),
		ocs.K("file_source", fileID),
		ocs.K("url", url),
		ocs.K("share_with", ""),
		ocs.K("mail_send", 0),
		ocs.K("hide_download", false),
		ocs.K("label", sh.Label),
	)
}
