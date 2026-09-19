package search

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

const (
	ocsSearchPrefixV1 = "/ocs/v1.php/search/providers"
	ocsSearchPrefixV2 = "/ocs/v2.php/search/providers"
	defaultLimit      = 20
)

// Handler serves OCS unified-search list and files search.
type Handler struct {
	Providers []Provider
	Version   ocs.Version
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
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, h.prefix()), "/")
	switch {
	case rest == "":
		h.list(w, r)
	case strings.HasSuffix(rest, "/search"):
		id := strings.TrimSuffix(rest, "/search")
		id = strings.Trim(id, "/")
		h.search(w, r, p.UID, id)
	default:
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
	}
}

func (h Handler) prefix() string {
	if h.Version == ocs.V1 {
		return ocsSearchPrefixV1
	}
	return ocsSearchPrefixV2
}

func (h Handler) list(w http.ResponseWriter, r *http.Request) {
	data := make([]any, 0, len(h.Providers))
	for _, p := range h.Providers {
		data = append(data, ocs.Obj(
			ocs.K("id", p.ID()),
			ocs.K("name", p.Name()),
			ocs.K("order", 5),
			ocs.K("appId", p.ID()),
			ocs.K("icon", ""),
		))
	}
	writeOCS(w, r, h.Version, 0, "", data)
}

func (h Handler) search(w http.ResponseWriter, r *http.Request, uid, providerID string) {
	prov := h.lookup(providerID)
	if prov == nil {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Provider not found", nil)
		return
	}
	term := strings.TrimSpace(r.URL.Query().Get("term"))
	hits, err := prov.Search(r.Context(), uid, term, parseLimit(r.URL.Query().Get("size")))
	if err != nil {
		writeOCS(w, r, h.Version, ocs.RespondServerError, "Internal Server Error", nil)
		return
	}
	entries := make([]any, 0, len(hits))
	for _, hit := range hits {
		entries = append(entries, ocs.Obj(
			ocs.K("thumbnailUrl", hit.ThumbnailURL),
			ocs.K("title", hit.Title),
			ocs.K("subline", hit.Subline),
			ocs.K("resourceUrl", resourceURL(r, hit.fileID)),
			ocs.K("icon", ""),
			ocs.K("rounded", false),
		))
	}
	writeOCS(w, r, h.Version, 0, "", ocs.Obj(
		ocs.K("name", prov.Name()),
		ocs.K("isPaginated", false),
		ocs.K("entries", entries),
	))
}

func (h Handler) lookup(id string) Provider {
	for _, p := range h.Providers {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

func parseLimit(raw string) int {
	if raw == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	if n > defaultLimit {
		return defaultLimit
	}
	return n
}

func resourceURL(r *http.Request, id int64) string {
	if id == 0 {
		return ""
	}
	return requestBase(r) + "/index.php/f/" + strconv.FormatInt(id, 10)
}

func requestBase(r *http.Request) string {
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil {
		host := r.Host
		if host == "localhost" || strings.HasPrefix(host, "127.") {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
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
