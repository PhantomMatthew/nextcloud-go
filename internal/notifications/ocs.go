package notifications

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

const (
	ocsPrefixV1 = "/ocs/v1.php/apps/notifications/api/v2/notifications"
	ocsPrefixV2 = "/ocs/v2.php/apps/notifications/api/v2/notifications"
)

// Handler serves OCS notifications list/get/delete.
type Handler struct {
	Store   Store
	Users   users.Store
	Version ocs.Version
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	id, collection, ok := parseNotifPath(r.URL.Path, h.prefix())
	if !ok {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
		return
	}
	switch {
	case collection && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.list(w, r, u.ID)
	case collection && r.Method == http.MethodDelete:
		h.deleteAll(w, r, u.ID)
	case !collection && r.Method == http.MethodGet:
		h.get(w, r, u.ID, id)
	case !collection && r.Method == http.MethodDelete:
		h.delete(w, r, u.ID, id)
	default:
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
	}
}

func (h Handler) prefix() string {
	if h.Version == ocs.V1 {
		return ocsPrefixV1
	}
	return ocsPrefixV2
}

func (h Handler) list(w http.ResponseWriter, r *http.Request, userID int64) {
	etag, err := h.Store.ListETag(r.Context(), userID)
	if err != nil {
		writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	quoted := `"` + etag + `"`
	if matchETag(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", quoted)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	items, err := h.Store.List(r.Context(), userID)
	if err != nil {
		writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	data := make([]any, 0, len(items))
	for i := range items {
		payload, err := notificationPayload(&items[i])
		if err != nil {
			writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
			return
		}
		data = append(data, payload)
	}
	w.Header().Set("ETag", quoted)
	writeOCS(w, r, h.Version, 0, "", data)
}

func (h Handler) get(w http.ResponseWriter, r *http.Request, userID, id int64) {
	n, err := h.Store.Get(r.Context(), userID, id)
	if err != nil {
		writeNotifErr(w, r, h.Version, err)
		return
	}
	payload, err := notificationPayload(n)
	if err != nil {
		writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	writeOCS(w, r, h.Version, 0, "", payload)
}

func (h Handler) delete(w http.ResponseWriter, r *http.Request, userID, id int64) {
	if err := h.Store.Delete(r.Context(), userID, id); err != nil {
		writeNotifErr(w, r, h.Version, err)
		return
	}
	writeOCS(w, r, h.Version, 0, "", []any{})
}

func (h Handler) deleteAll(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := h.Store.DeleteAll(r.Context(), userID); err != nil {
		writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	writeOCS(w, r, h.Version, 0, "", []any{})
}

func notificationPayload(n *Notification) (any, error) {
	params, err := decodeJSON(n.SubjectRichParameters, "{}")
	if err != nil {
		return nil, err
	}
	msgParams, err := decodeJSON(n.MessageRichParameters, "[]")
	if err != nil {
		return nil, err
	}
	return ocs.Obj(
		ocs.K("notification_id", n.ID),
		ocs.K("app", n.App),
		ocs.K("user", n.UserUID),
		ocs.K("datetime", iso8601(n.CreatedAt)),
		ocs.K("object_type", n.ObjectType),
		ocs.K("object_id", n.ObjectID),
		ocs.K("subject", n.Subject),
		ocs.K("subjectRich", n.SubjectRich),
		ocs.K("subjectRichParameters", params),
		ocs.K("message", n.Message),
		ocs.K("messageRich", n.MessageRich),
		ocs.K("messageRichParameters", msgParams),
		ocs.K("link", n.Link),
		ocs.K("icon", n.Icon),
		ocs.K("shouldNotify", n.ShouldNotify),
		ocs.K("actions", []any{}),
	), nil
}

func parseNotifPath(path, prefix string) (id int64, collection, ok bool) {
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

func matchETag(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		part = strings.Trim(part, `"`)
		if part == etag {
			return true
		}
	}
	return false
}

func iso8601(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05-07:00")
}

func writeNotifErr(w http.ResponseWriter, r *http.Request, version ocs.Version, err error) {
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
