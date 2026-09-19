package activity

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
	ocsPrefixV1 = "/ocs/v1.php/apps/activity/api/v2/activity"
	ocsPrefixV2 = "/ocs/v2.php/apps/activity/api/v2/activity"
)

// Handler serves OCS activity list.
type Handler struct {
	Store   Store
	Users   users.Store
	Version ocs.Version
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
		return
	}
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
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, h.prefix()), "/")
	if rest != "" {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
		return
	}
	h.list(w, r, u.ID)
}

func (h Handler) prefix() string {
	if h.Version == ocs.V1 {
		return ocsPrefixV1
	}
	return ocsPrefixV2
}

func (h Handler) list(w http.ResponseWriter, r *http.Request, userID int64) {
	q := r.URL.Query()
	since, err := strconv.ParseInt(q.Get("since"), 10, 64)
	if err != nil {
		since = 0
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = 0
	}
	sort := q.Get("sort")
	items, err := h.Store.List(r.Context(), userID, since, limit, sort)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			writeOCS(w, r, h.Version, 400, "Invalid request", nil)
			return
		}
		writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	data := make([]any, 0, len(items))
	for i := range items {
		payload, err := eventPayload(&items[i])
		if err != nil {
			writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
			return
		}
		data = append(data, payload)
	}
	if len(items) > 0 {
		w.Header().Set("X-Activity-Last-Given", strconv.FormatInt(items[len(items)-1].ID, 10))
	}
	writeOCS(w, r, h.Version, 0, "", data)
}

func eventPayload(e *Event) (any, error) {
	params, err := decodeJSON(e.SubjectRichParameters, "{}")
	if err != nil {
		return nil, err
	}
	rich := []any{e.SubjectRich, params}
	if e.SubjectRich == "" {
		rich = []any{}
	}
	return ocs.Obj(
		ocs.K("activity_id", e.ID),
		ocs.K("datetime", iso8601(e.CreatedAt)),
		ocs.K("app", e.App),
		ocs.K("type", e.Type),
		ocs.K("user", e.ActorUID),
		ocs.K("subject", e.Subject),
		ocs.K("subject_rich", rich),
		ocs.K("message", e.Message),
		ocs.K("message_rich", []any{}),
		ocs.K("icon", e.Icon),
		ocs.K("link", e.Link),
		ocs.K("object_type", e.ObjectType),
		ocs.K("object_id", e.ObjectID),
		ocs.K("object_name", e.ObjectName),
		ocs.K("previews", []any{}),
	), nil
}

func iso8601(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05-07:00")
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
