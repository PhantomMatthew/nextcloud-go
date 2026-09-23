package console

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/status"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/version"
)

//go:embed ui
var uiFS embed.FS

// UserStore is the users.Store read subset the console needs.
type UserStore interface {
	List(ctx context.Context, limit, offset int) ([]users.User, error)
	Count(ctx context.Context) (int64, error)
}

// GroupLookup resolves the gids a uid belongs to (users.Store).
type GroupLookup interface {
	UserGroupGIDs(ctx context.Context, uid string) ([]string, error)
}

// JobsStore is the jobs.SQLStore read subset the console needs.
type JobsStore interface {
	ListRecent(ctx context.Context, limit int) ([]jobs.Row, error)
}

// DBProbe reports database health for the status view (database.DB).
type DBProbe interface {
	Ping(ctx context.Context) error
	Dialect() database.Dialect
}

// Handler serves the embedded admin console: the page and its two assets,
// plus the three read-only JSON endpoints the page renders. All routes are
// GET/HEAD; anything else answers 405.
type Handler struct {
	Users      UserStore
	Jobs       JobsStore
	DB         DBProbe
	Cfg        *config.Config
	InstanceID string
	Status     status.Provider
}

// ServeHTTP dispatches the console namespace: the shell at /console and
// /console/, the two embedded assets, and /console/api/{status,users,jobs}.
// Any other /console/* path 404s — there is no SPA fallback here.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/console", "/console/":
		h.serveAsset(w, r, "ui/index.html", "text/html; charset=utf-8")
	case "/console/console.js":
		h.serveAsset(w, r, "ui/console.js", "text/javascript; charset=utf-8")
	case "/console/console.css":
		h.serveAsset(w, r, "ui/console.css", "text/css; charset=utf-8")
	case "/console/api/status":
		h.serveStatus(w, r)
	case "/console/api/users":
		h.serveUsers(w, r)
	case "/console/api/jobs":
		h.serveJobs(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serveAsset writes one embedded file; the assets never change between
// binary builds, so they are always revalidated (no-cache).
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	b, err := uiFS.ReadFile(name)
	if err != nil {
		writeError(w, r, "asset unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

type statusPayload struct {
	Version        string      `json:"version"`
	VersionString  string      `json:"versionstring"`
	Edition        string      `json:"edition"`
	ProductName    string      `json:"productname"`
	InstanceID     string      `json:"instanceID"`
	Installed      bool        `json:"installed"`
	Maintenance    bool        `json:"maintenance"`
	NeedsDBUpgrade bool        `json:"needsDbUpgrade"`
	DB             dbInfo      `json:"db"`
	Storage        storageInfo `json:"storage"`
	Encryption     bool        `json:"encryption"`
	Metrics        bool        `json:"metrics"`
	Previews       bool        `json:"previews"`
	Plugins        bool        `json:"plugins"`
	Users          int64       `json:"users"`
	Time           string      `json:"time"`
}

type dbInfo struct {
	Dialect   string `json:"dialect"`
	Reachable bool   `json:"reachable"`
}

type storageInfo struct {
	Default  string        `json:"default"`
	Backends []backendInfo `json:"backends"`
}

type backendInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func (h *Handler) serveStatus(w http.ResponseWriter, r *http.Request) {
	p := statusPayload{
		Version:        version.String(),
		VersionString:  version.VersionString,
		Edition:        version.Edition,
		ProductName:    version.ProductName,
		InstanceID:     h.InstanceID,
		Installed:      h.Status.Installed,
		Maintenance:    h.Status.Maintenance,
		NeedsDBUpgrade: h.Status.NeedsDBUpgrade,
		Storage:        storageInfo{Backends: []backendInfo{}},
		Time:           time.Now().UTC().Format(time.RFC3339),
	}
	if h.DB != nil {
		p.DB.Dialect = string(h.DB.Dialect())
		p.DB.Reachable = h.DB.Ping(r.Context()) == nil
	}
	if h.Cfg != nil {
		p.Storage.Default = h.Cfg.Storage.DefaultBackend
		for name, b := range h.Cfg.Storage.Backends {
			p.Storage.Backends = append(p.Storage.Backends, backendInfo{Name: name, Type: b.Type})
		}
		slices.SortFunc(p.Storage.Backends, func(a, b backendInfo) int {
			if a.Name < b.Name {
				return -1
			}
			if a.Name > b.Name {
				return 1
			}
			return 0
		})
		p.Encryption = h.Cfg.Encryption.Enabled
		p.Metrics = h.Cfg.Observability.MetricsEnabled
		p.Previews = h.Cfg.Previews.Enabled
		p.Plugins = h.Cfg.Plugin.Enabled
	}
	if h.Users != nil {
		n, err := h.Users.Count(r.Context())
		if err != nil {
			writeError(w, r, "user count failed")
			return
		}
		p.Users = n
	}
	writeJSON(w, http.StatusOK, p)
}

type usersPayload struct {
	Total int64      `json:"total"`
	Users []userInfo `json:"users"`
}

type userInfo struct {
	UID         string `json:"uid"`
	DisplayName string `json:"displayname"`
	Email       string `json:"email"`
	Enabled     bool   `json:"enabled"`
	QuotaBytes  *int64 `json:"quota_bytes"`
}

// serveUsers lists accounts a page at a time. Per-user groups are
// deliberately omitted in v1 — resolving them is an N+1 against the groups
// tables (ADR-0080).
func (h *Handler) serveUsers(w http.ResponseWriter, r *http.Request) {
	if h.Users == nil {
		writeError(w, r, "users store unavailable")
		return
	}
	limit, offset := pageParams(r)
	total, err := h.Users.Count(r.Context())
	if err != nil {
		writeError(w, r, "user count failed")
		return
	}
	list, err := h.Users.List(r.Context(), limit, offset)
	if err != nil {
		writeError(w, r, "user list failed")
		return
	}
	out := usersPayload{Total: total, Users: make([]userInfo, 0, len(list))}
	for _, u := range list {
		out.Users = append(out.Users, userInfo{
			UID:         u.UID,
			DisplayName: u.DisplayName,
			Email:       u.Email,
			Enabled:     u.Enabled,
			QuotaBytes:  u.QuotaBytes,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type jobsPayload struct {
	Jobs []jobInfo `json:"jobs"`
}

type jobInfo struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	RunAt       string  `json:"run_at"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
	LastError   string  `json:"last_error"`
	Attempts    int     `json:"attempts"`
	State       string  `json:"state"`
}

// jobState derives the display state from the row fields: completed wins
// (a retried-then-finished job is done), then an error text means failed,
// then a start timestamp means running, otherwise the row waits queued.
func jobState(r jobs.Row) string {
	switch {
	case r.CompletedAt != 0:
		return "done"
	case r.LastError != "":
		return "failed"
	case r.StartedAt != 0:
		return "running"
	default:
		return "queued"
	}
}

// serveJobs lists the newest jobs rows, any state. Timestamps are RFC3339
// (UTC); the nullable started/completed columns serialize as null (ADR-0080).
func (h *Handler) serveJobs(w http.ResponseWriter, r *http.Request) {
	if h.Jobs == nil {
		writeError(w, r, "jobs store unavailable")
		return
	}
	limit, _ := pageParams(r)
	rows, err := h.Jobs.ListRecent(r.Context(), limit)
	if err != nil {
		writeError(w, r, "job list failed")
		return
	}
	out := jobsPayload{Jobs: make([]jobInfo, 0, len(rows))}
	for _, row := range rows {
		out.Jobs = append(out.Jobs, jobInfo{
			ID:          row.ID,
			Name:        row.Name,
			RunAt:       msTime(row.RunAt),
			StartedAt:   msTimePtr(row.StartedAt),
			CompletedAt: msTimePtr(row.CompletedAt),
			LastError:   row.LastError,
			Attempts:    row.Attempts,
			State:       jobState(row),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func msTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func msTimePtr(ms int64) *string {
	if ms == 0 {
		return nil
	}
	s := msTime(ms)
	return &s
}

// pageParams reads limit/offset with the console's defaults and clamps:
// limit defaults to 50 and stays in [1,200], offset never goes negative.
// Unparsable values fall back to the default rather than erroring.
func pageParams(r *http.Request) (limit, offset int) {
	limit, offset = 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	limit = min(max(limit, 1), 200)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}
	return limit, max(offset, 0)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "marshal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// writeError answers internal failures in the request's own shape: JSON for
// the api endpoints, a plain status page everywhere else.
func writeError(w http.ResponseWriter, r *http.Request, msg string) {
	if isAPIPath(r.URL.Path) {
		writeJSON(w, http.StatusInternalServerError, errorPayload{Error: msg})
		return
	}
	http.Error(w, msg, http.StatusInternalServerError)
}

// writeAuthError answers 401/403: JSON for /console/api/* (the page's fetch
// layer renders it), a tiny HTML page linking the login form for page and
// asset GETs (an anonymous browser lands somewhere useful).
func writeAuthError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if isAPIPath(r.URL.Path) {
		writeJSON(w, code, errorPayload{Error: msg})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(authPage(code, msg)))
}

type errorPayload struct {
	Error string `json:"error"`
}

func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/console/api/")
}

// authPage renders the minimal HTML auth-error document. title and msg are
// always internal constants, never request input.
func authPage(code int, msg string) string {
	title := http.StatusText(code)
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ncgo console — ` + title + `</title>
<style>body{background:#1b1b1e;color:#e4e4e7;font-family:system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}main{text-align:center}a{color:#7aa2f7}</style>
</head>
<body>
<main>
<h1>` + title + `</h1>
<p>` + msg + `.</p>
<p><a href="/index.php/login">Sign in</a></p>
</main>
</body>
</html>
`
}
