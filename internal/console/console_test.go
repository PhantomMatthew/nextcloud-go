package console

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/notifications"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/status"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

const jobRunAtMs = 1746100800000 // 2025-05-01T12:00:00Z

type fakeUserStore struct {
	total            int64
	list             []users.User
	gotLimit, gotOff int
}

func (f *fakeUserStore) List(_ context.Context, limit, offset int) ([]users.User, error) {
	f.gotLimit, f.gotOff = limit, offset
	return f.list, nil
}

func (f *fakeUserStore) Count(_ context.Context) (int64, error) { return f.total, nil }

type fakeGroupLookup struct {
	gids map[string][]string
	err  error
}

func (f fakeGroupLookup) UserGroupGIDs(_ context.Context, uid string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.gids[uid], nil
}

type fakeJobsStore struct {
	rows     []jobs.Row
	gotLimit int
}

func (f *fakeJobsStore) ListRecent(_ context.Context, limit int) ([]jobs.Row, error) {
	f.gotLimit = limit
	return f.rows, nil
}

type fakeNotifsStore struct {
	items    []notifications.Notification
	gotLimit int
}

func (f *fakeNotifsStore) ListRecent(_ context.Context, limit int) ([]notifications.Notification, error) {
	f.gotLimit = limit
	return f.items, nil
}

type fakeDBProbe struct {
	dialect database.Dialect
	pingErr error
}

func (f fakeDBProbe) Ping(_ context.Context) error { return f.pingErr }
func (f fakeDBProbe) Dialect() database.Dialect    { return f.dialect }

func testHandler() (*Handler, *fakeUserStore, *fakeJobsStore, *fakeNotifsStore) {
	quota := int64(1073741824)
	fu := &fakeUserStore{
		total: 2,
		list: []users.User{
			{UID: "admin", DisplayName: "Admin", Email: "admin@example.com", Enabled: true, QuotaBytes: &quota},
			{UID: "bob", DisplayName: "Bob", Enabled: false},
		},
	}
	fj := &fakeJobsStore{rows: []jobs.Row{
		{ID: 4, Name: "failed.job", RunAt: jobRunAtMs, LastError: "boom", Attempts: 2},
		{ID: 3, Name: "done.job", RunAt: jobRunAtMs, StartedAt: jobRunAtMs, CompletedAt: jobRunAtMs + 60000},
		{ID: 2, Name: "running.job", RunAt: jobRunAtMs, StartedAt: jobRunAtMs},
		{ID: 1, Name: "queued.job", RunAt: jobRunAtMs},
	}}
	fn := &fakeNotifsStore{items: []notifications.Notification{
		{
			ID: 2, UserID: 2, App: "files_sharing", UserUID: "bob", ObjectType: "share", ObjectID: "ocinternal:9",
			Subject: "You received b.txt as a share by Alice", ShouldNotify: true,
			CreatedAt: time.UnixMilli(jobRunAtMs + 1000).UTC(),
		},
		{
			ID: 1, UserID: 1, App: "files_sharing", UserUID: "alice", ObjectType: "share", ObjectID: "ocinternal:7",
			Subject: "You received a.txt as a share by Bob", Message: "hello", Link: "https://example.com/f/7", Icon: "icon.svg",
			ShouldNotify: true, CreatedAt: time.UnixMilli(jobRunAtMs).UTC(),
		},
	}}
	reg := observability.NewRegistry()
	// alpha: host-call traffic across two functions with one error class, two
	// capability denials, and three latency observations.
	hc := func(result string) {
		reg.IncCounter(observability.MetricPluginHostCallsTotal,
			observability.Label{Name: "plugin", Value: "alpha"},
			observability.Label{Name: "function", Value: "cache_get"},
			observability.Label{Name: "result", Value: result})
	}
	hc("ok")
	hc("ok")
	reg.IncCounter(observability.MetricPluginHostCallsTotal,
		observability.Label{Name: "plugin", Value: "alpha"},
		observability.Label{Name: "function", Value: "cache_set"},
		observability.Label{Name: "result", Value: "internal"})
	for i := 0; i < 2; i++ {
		reg.IncCounter(observability.MetricPluginCapabilityDenialsTotal,
			observability.Label{Name: "plugin", Value: "alpha"},
			observability.Label{Name: "function", Value: "cache_set"})
	}
	hd := func(seconds float64, function string) {
		reg.ObserveHistogram(observability.MetricPluginHostCallDurationSeconds, seconds,
			observability.Label{Name: "plugin", Value: "alpha"},
			observability.Label{Name: "function", Value: function})
	}
	hd(0.01, "cache_get")
	hd(0.03, "cache_get")
	hd(0.5, "cache_set")
	// alpha: memory high-water and two limit breaches on top of host traffic.
	reg.SetGaugeMax(observability.MetricPluginMemoryHighWaterBytes, 3<<20,
		observability.Label{Name: "plugin", Value: "alpha"})
	reg.SetGaugeMax(observability.MetricPluginMemoryHighWaterBytes, 1<<20, // lower: ignored
		observability.Label{Name: "plugin", Value: "alpha"})
	for i := 0; i < 2; i++ {
		reg.IncCounter(observability.MetricPluginMemoryLimitExceededTotal,
			observability.Label{Name: "plugin", Value: "alpha"})
	}
	// beta: entry-point traffic only (no host families at all).
	ec := func(entry, result string) {
		reg.IncCounter(observability.MetricPluginEntryCallsTotal,
			observability.Label{Name: "plugin", Value: "beta"},
			observability.Label{Name: "entry", Value: entry},
			observability.Label{Name: "result", Value: result})
	}
	ec("Call", "ok")
	ec("Call", "ok")
	ec("Call", "ok")
	ec("onTransfer", "timeout")
	ed := func(seconds float64) {
		reg.ObserveHistogram(observability.MetricPluginEntryCallDurationSeconds, seconds,
			observability.Label{Name: "plugin", Value: "beta"},
			observability.Label{Name: "entry", Value: "Call"})
	}
	ed(0.002)
	ed(0.2)
	// gamma: storage bytes only — the zero-family plugin.
	reg.AddCounter(observability.MetricPluginStorageBytesTotal, 100,
		observability.Label{Name: "plugin", Value: "gamma"},
		observability.Label{Name: "op", Value: "write"},
		observability.Label{Name: "scope", Value: "user"})
	reg.AddCounter(observability.MetricPluginStorageBytesTotal, 50,
		observability.Label{Name: "plugin", Value: "gamma"},
		observability.Label{Name: "op", Value: "read"},
		observability.Label{Name: "scope", Value: "system"})
	// delta: memory gauge only — a plugin appearing in just one of the new
	// families still gets a row, with every other family zero.
	reg.SetGaugeMax(observability.MetricPluginMemoryHighWaterBytes, 768<<10,
		observability.Label{Name: "plugin", Value: "delta"})
	h := &Handler{
		Users:  fu,
		Jobs:   fj,
		Notifs: fn,
		DB:     fakeDBProbe{dialect: database.DialectSQLite},
		Cfg: &config.Config{
			Storage: config.StorageConfig{
				DefaultBackend: "local",
				Backends: map[string]config.BackendConfig{
					"s3backup": {Type: "s3"},
					"local":    {Type: "localfs"},
				},
			},
			Previews:      config.PreviewsConfig{Enabled: true},
			Observability: config.ObservabilityConfig{MetricsEnabled: true},
		},
		InstanceID: "octestinstance",
		Status:     status.Provider{Installed: true},
		Metrics:    reg,
	}
	return h, fu, fj, fn
}

// gated wraps the handler the way mountRoutes does: an injected (or absent)
// principal stands in for the auth middleware, then RequireAdmin.
func gated(h http.Handler, groups GroupLookup, p *auth.Principal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p != nil {
			r = r.WithContext(auth.WithUser(r.Context(), p))
		}
		RequireAdmin(groups)(h).ServeHTTP(w, r)
	})
}

func adminChain(h http.Handler) http.Handler {
	return gated(h, fakeGroupLookup{gids: map[string][]string{
		"admin": {users.AdminGroupGID, "staff"},
		"bob":   {"staff"},
	}}, &auth.Principal{UID: "admin", Enabled: true})
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), method, path, nil))
	return rr
}

func TestConsoleAuthGate(t *testing.T) {
	h, _, _, _ := testHandler()
	tests := []struct {
		name       string
		chain      http.Handler
		method     string
		path       string
		wantStatus int
		wantCT     string
		wantInBody string
	}{
		{name: "anonymous page", chain: gated(h, fakeGroupLookup{}, nil), path: "/console", wantStatus: http.StatusUnauthorized, wantCT: "text/html", wantInBody: "/index.php/login"},
		{name: "anonymous asset", chain: gated(h, fakeGroupLookup{}, nil), path: "/console/console.js", wantStatus: http.StatusUnauthorized, wantCT: "text/html", wantInBody: "/index.php/login"},
		{name: "anonymous api", chain: gated(h, fakeGroupLookup{}, nil), path: "/console/api/status", wantStatus: http.StatusUnauthorized, wantCT: "application/json", wantInBody: `"error"`},
		{name: "non-admin page", chain: gated(h, fakeGroupLookup{gids: map[string][]string{"bob": {"staff"}}}, &auth.Principal{UID: "bob"}), path: "/console", wantStatus: http.StatusForbidden, wantCT: "text/html", wantInBody: "/index.php/login"},
		{name: "non-admin api", chain: gated(h, fakeGroupLookup{gids: map[string][]string{"bob": {"staff"}}}, &auth.Principal{UID: "bob"}), path: "/console/api/users", wantStatus: http.StatusForbidden, wantCT: "application/json", wantInBody: `"error"`},
		{name: "group lookup error", chain: gated(h, fakeGroupLookup{err: errors.New("db down")}, &auth.Principal{UID: "admin"}), path: "/console/api/jobs", wantStatus: http.StatusInternalServerError, wantCT: "application/json"},
		{name: "admin page", chain: adminChain(h), path: "/console", wantStatus: http.StatusOK, wantCT: "text/html", wantInBody: "ncgo admin console"},
		{name: "admin trailing slash", chain: adminChain(h), path: "/console/", wantStatus: http.StatusOK, wantCT: "text/html"},
		{name: "admin js", chain: adminChain(h), path: "/console/console.js", wantStatus: http.StatusOK, wantCT: "text/javascript", wantInBody: "loadJSON"},
		{name: "admin css", chain: adminChain(h), path: "/console/console.css", wantStatus: http.StatusOK, wantCT: "text/css"},
		{name: "unknown asset", chain: adminChain(h), path: "/console/missing.js", wantStatus: http.StatusNotFound},
		{name: "unknown api", chain: adminChain(h), path: "/console/api/nope", wantStatus: http.StatusNotFound},
		{name: "post page", chain: adminChain(h), method: http.MethodPost, path: "/console", wantStatus: http.StatusMethodNotAllowed},
		{name: "post api", chain: adminChain(h), method: http.MethodPost, path: "/console/api/jobs", wantStatus: http.StatusMethodNotAllowed},
		{name: "put asset", chain: adminChain(h), method: http.MethodPut, path: "/console/console.js", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rr := do(t, tt.chain, method, tt.path)
			if rr.Code != tt.wantStatus {
				t.Fatalf("%s %s: status = %d, want %d", method, tt.path, rr.Code, tt.wantStatus)
			}
			if tt.wantCT != "" {
				if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, tt.wantCT) {
					t.Errorf("Content-Type = %q, want prefix %q", ct, tt.wantCT)
				}
			}
			if tt.wantInBody != "" && !strings.Contains(rr.Body.String(), tt.wantInBody) {
				t.Errorf("body missing %q: %s", tt.wantInBody, rr.Body.String())
			}
			if tt.wantStatus == http.StatusMethodNotAllowed {
				if allow := rr.Header().Get("Allow"); allow != "GET, HEAD" {
					t.Errorf("Allow = %q, want GET, HEAD", allow)
				}
			}
			if tt.wantStatus == http.StatusOK {
				if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
					t.Errorf("Cache-Control = %q, want no-cache", cc)
				}
			}
		})
	}
}

func TestConsoleHeadAsset(t *testing.T) {
	h, _, _, _ := testHandler()
	rr := do(t, adminChain(h), http.MethodHead, "/console/console.js")
	if rr.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD body = %d bytes, want 0", rr.Body.Len())
	}
}

func TestConsoleAuthMiddleware(t *testing.T) {
	h, _, _, _ := testHandler()
	// No verifiers configured: every request fails authentication and the
	// console failure writer answers — JSON on api paths, login-link HTML
	// on the page, never an empty WebDAV-style challenge.
	chain := Auth(auth.MiddlewareConfig{})(RequireAdmin(fakeGroupLookup{})(h))
	rr := do(t, chain, http.MethodGet, "/console")
	if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "/index.php/login") {
		t.Fatalf("anonymous page = %d %q", rr.Code, rr.Body.String())
	}
	rr = do(t, chain, http.MethodGet, "/console/api/status")
	if rr.Code != http.StatusUnauthorized || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("anonymous api = %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}

	// A passing verifier flows through to the group gate.
	chain = Auth(auth.MiddlewareConfig{Verifier: okVerifier{uid: "admin"}})(gated(h, fakeGroupLookup{gids: map[string][]string{"admin": {users.AdminGroupGID}}}, nil))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console/api/status", nil)
	req.SetBasicAuth("admin", "x")
	rr = httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("basic-auth admin = %d", rr.Code)
	}
}

type okVerifier struct{ uid string }

func (v okVerifier) Verify(_ context.Context, uid, _ string) (*auth.Principal, error) {
	if uid == v.uid {
		return &auth.Principal{UID: uid, Enabled: true, AuthMethod: auth.AuthMethodBasic}, nil
	}
	return nil, auth.ErrInvalidCredentials
}

func TestConsoleStatus(t *testing.T) {
	h, _, _, _ := testHandler()
	rr := do(t, adminChain(h), http.MethodGet, "/console/api/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var p struct {
		Version    string `json:"version"`
		InstanceID string `json:"instanceID"`
		Installed  bool   `json:"installed"`
		DB         struct {
			Dialect   string `json:"dialect"`
			Reachable bool   `json:"reachable"`
		} `json:"db"`
		Storage struct {
			Default  string `json:"default"`
			Backends []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"backends"`
		} `json:"storage"`
		Encryption bool   `json:"encryption"`
		Metrics    bool   `json:"metrics"`
		Previews   bool   `json:"previews"`
		Plugins    bool   `json:"plugins"`
		Users      int64  `json:"users"`
		Time       string `json:"time"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != "26.0.0.6" || p.InstanceID != "octestinstance" || !p.Installed {
		t.Errorf("version/instance/installed = %+v", p)
	}
	if p.DB.Dialect != "sqlite" || !p.DB.Reachable {
		t.Errorf("db = %+v", p.DB)
	}
	if p.Storage.Default != "local" || len(p.Storage.Backends) != 2 ||
		p.Storage.Backends[0].Name != "local" || p.Storage.Backends[1].Name != "s3backup" {
		t.Errorf("storage (must be name-sorted) = %+v", p.Storage)
	}
	if p.Encryption || !p.Metrics || !p.Previews || p.Plugins {
		t.Errorf("flags = %+v", p)
	}
	if p.Users != 2 {
		t.Errorf("users = %d", p.Users)
	}
	if _, err := time.Parse(time.RFC3339, p.Time); err != nil {
		t.Errorf("time %q not RFC3339: %v", p.Time, err)
	}
}

func TestConsoleUsersPagination(t *testing.T) {
	h, fu, _, _ := testHandler()
	tests := []struct {
		query      string
		wantLimit  int
		wantOffset int
	}{
		{query: "", wantLimit: 50, wantOffset: 0},
		{query: "?limit=10&offset=20", wantLimit: 10, wantOffset: 20},
		{query: "?limit=0", wantLimit: 1},
		{query: "?limit=9999", wantLimit: 200},
		{query: "?limit=-3", wantLimit: 1},
		{query: "?offset=-5", wantLimit: 50, wantOffset: 0},
		{query: "?limit=abc&offset=xyz", wantLimit: 50, wantOffset: 0},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			rr := do(t, adminChain(h), http.MethodGet, "/console/api/users"+tt.query)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if fu.gotLimit != tt.wantLimit || fu.gotOff != tt.wantOffset {
				t.Errorf("limit/offset = %d/%d, want %d/%d", fu.gotLimit, fu.gotOff, tt.wantLimit, tt.wantOffset)
			}
		})
	}
}

func TestConsoleUsersPayload(t *testing.T) {
	h, _, _, _ := testHandler()
	rr := do(t, adminChain(h), http.MethodGet, "/console/api/users")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var p struct {
		Total int64 `json:"total"`
		Users []struct {
			UID        string `json:"uid"`
			Email      string `json:"email"`
			Enabled    bool   `json:"enabled"`
			QuotaBytes *int64 `json:"quota_bytes"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Total != 2 || len(p.Users) != 2 {
		t.Fatalf("payload = %+v", p)
	}
	if p.Users[0].UID != "admin" || p.Users[0].QuotaBytes == nil || *p.Users[0].QuotaBytes != 1073741824 {
		t.Errorf("admin row = %+v", p.Users[0])
	}
	if p.Users[1].UID != "bob" || p.Users[1].QuotaBytes != nil || p.Users[1].Enabled {
		t.Errorf("bob row (nil quota, disabled) = %+v", p.Users[1])
	}
}

func TestConsoleJobsPayload(t *testing.T) {
	h, _, fj, _ := testHandler()
	rr := do(t, adminChain(h), http.MethodGet, "/console/api/jobs")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if fj.gotLimit != 50 {
		t.Errorf("jobs limit = %d, want default 50", fj.gotLimit)
	}
	var p struct {
		Jobs []struct {
			ID          int64   `json:"id"`
			Name        string  `json:"name"`
			RunAt       string  `json:"run_at"`
			StartedAt   *string `json:"started_at"`
			CompletedAt *string `json:"completed_at"`
			LastError   string  `json:"last_error"`
			Attempts    int     `json:"attempts"`
			State       string  `json:"state"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Jobs) != 4 {
		t.Fatalf("jobs = %+v", p.Jobs)
	}
	wantStates := []string{"failed", "done", "running", "queued"} // id DESC as seeded
	for i, want := range wantStates {
		if p.Jobs[i].State != want {
			t.Errorf("jobs[%d].State = %q, want %q", i, p.Jobs[i].State, want)
		}
	}
	if p.Jobs[0].LastError != "boom" || p.Jobs[0].Attempts != 2 {
		t.Errorf("failed job = %+v", p.Jobs[0])
	}
	if p.Jobs[1].CompletedAt == nil || *p.Jobs[1].CompletedAt != "2025-05-01T12:01:00Z" {
		t.Errorf("done job completed_at = %+v", p.Jobs[1].CompletedAt)
	}
	if p.Jobs[2].StartedAt == nil || *p.Jobs[2].StartedAt != "2025-05-01T12:00:00Z" {
		t.Errorf("running job started_at = %+v", p.Jobs[2].StartedAt)
	}
	if p.Jobs[3].StartedAt != nil || p.Jobs[3].CompletedAt != nil {
		t.Errorf("queued job timestamps must be null = %+v", p.Jobs[3])
	}
	if p.Jobs[3].RunAt != "2025-05-01T12:00:00Z" {
		t.Errorf("run_at = %q", p.Jobs[3].RunAt)
	}

	rr = do(t, adminChain(h), http.MethodGet, "/console/api/jobs?limit=5")
	if fj.gotLimit != 5 || rr.Code != http.StatusOK {
		t.Errorf("jobs?limit=5: gotLimit = %d, status = %d", fj.gotLimit, rr.Code)
	}
}

func TestConsoleNotifsPayload(t *testing.T) {
	h, _, _, fn := testHandler()
	rr := do(t, adminChain(h), http.MethodGet, "/console/api/notifications")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if fn.gotLimit != 50 {
		t.Errorf("notifications limit = %d, want default 50", fn.gotLimit)
	}
	var p struct {
		Notifications []struct {
			ID         int64  `json:"id"`
			User       string `json:"user"`
			App        string `json:"app"`
			ObjectType string `json:"object_type"`
			ObjectID   string `json:"object_id"`
			Subject    string `json:"subject"`
			Message    string `json:"message"`
			Link       string `json:"link"`
			Icon       string `json:"icon"`
			CreatedAt  string `json:"created_at"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Notifications) != 2 {
		t.Fatalf("notifications = %+v", p.Notifications)
	}
	first := p.Notifications[0]
	if first.ID != 2 || first.User != "bob" || first.App != "files_sharing" ||
		first.ObjectType != "share" || first.ObjectID != "ocinternal:9" ||
		first.Subject != "You received b.txt as a share by Alice" {
		t.Errorf("first = %+v", first)
	}
	if first.CreatedAt != "2025-05-01T12:00:01Z" {
		t.Errorf("created_at = %q", first.CreatedAt)
	}
	second := p.Notifications[1]
	if second.ID != 1 || second.User != "alice" || second.Message != "hello" ||
		second.Link != "https://example.com/f/7" || second.Icon != "icon.svg" {
		t.Errorf("second = %+v", second)
	}

	rr = do(t, adminChain(h), http.MethodGet, "/console/api/notifications?limit=5")
	if fn.gotLimit != 5 || rr.Code != http.StatusOK {
		t.Errorf("notifications?limit=5: gotLimit = %d, status = %d", fn.gotLimit, rr.Code)
	}
}

func TestConsoleNotifsNilStore(t *testing.T) {
	h, _, _, _ := testHandler()
	h.Notifs = nil
	rr := do(t, adminChain(h), http.MethodGet, "/console/api/notifications")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("nil notifs store = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"error"`) || !strings.Contains(rr.Body.String(), "notifications store unavailable") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

type pluginRow struct {
	Plugin               string  `json:"plugin"`
	HostCalls            int64   `json:"host_calls"`
	HostErrors           int64   `json:"host_errors"`
	EntryCalls           int64   `json:"entry_calls"`
	EntryErrors          int64   `json:"entry_errors"`
	HostCallMeanSeconds  float64 `json:"host_call_mean_seconds"`
	HostCallP50Seconds   float64 `json:"host_call_p50_seconds"`
	HostCallP95Seconds   float64 `json:"host_call_p95_seconds"`
	HostCallP99Seconds   float64 `json:"host_call_p99_seconds"`
	EntryCallMeanSeconds float64 `json:"entry_call_mean_seconds"`
	EntryCallP50Seconds  float64 `json:"entry_call_p50_seconds"`
	EntryCallP95Seconds  float64 `json:"entry_call_p95_seconds"`
	EntryCallP99Seconds  float64 `json:"entry_call_p99_seconds"`
	StorageBytes         int64   `json:"storage_bytes"`
	CapabilityDenials    int64   `json:"capability_denials"`
	MemoryHighWaterBytes int64   `json:"memory_high_water_bytes"`
	MemoryLimitExceeded  int64   `json:"memory_limit_exceeded"`
}

func TestConsolePluginsPayload(t *testing.T) {
	h, _, _, _ := testHandler()
	rr := do(t, adminChain(h), http.MethodGet, "/console/api/plugins")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var p struct {
		Plugins []pluginRow `json:"plugins"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Plugins) != 4 {
		t.Fatalf("plugins = %+v", p.Plugins)
	}
	// Rows are id-sorted: alpha, beta, delta, gamma.
	alpha, beta, delta, gamma := p.Plugins[0], p.Plugins[1], p.Plugins[2], p.Plugins[3]
	if alpha.Plugin != "alpha" || beta.Plugin != "beta" || delta.Plugin != "delta" || gamma.Plugin != "gamma" {
		t.Fatalf("plugin order = %q, %q, %q, %q", alpha.Plugin, beta.Plugin, delta.Plugin, gamma.Plugin)
	}

	// alpha: 3 host calls summed over function (2 ok + 1 internal), 2
	// denials, latency merged from the per-function series (count 3,
	// sum 0.54), a 3 MiB memory high-water (the 1 MiB set-max lost), and 2
	// limit breaches.
	if alpha.HostCalls != 3 || alpha.HostErrors != 1 {
		t.Errorf("alpha host calls/errors = %d/%d, want 3/1", alpha.HostCalls, alpha.HostErrors)
	}
	if alpha.EntryCalls != 0 || alpha.EntryErrors != 0 || alpha.StorageBytes != 0 {
		t.Errorf("alpha zero families = %+v", alpha)
	}
	if alpha.CapabilityDenials != 2 {
		t.Errorf("alpha denials = %d, want 2", alpha.CapabilityDenials)
	}
	if alpha.MemoryHighWaterBytes != 3<<20 {
		t.Errorf("alpha memory high-water = %d, want %d", alpha.MemoryHighWaterBytes, 3<<20)
	}
	if alpha.MemoryLimitExceeded != 2 {
		t.Errorf("alpha memory limit exceeded = %d, want 2", alpha.MemoryLimitExceeded)
	}
	approx := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if !approx(alpha.HostCallMeanSeconds, 0.18) {
		t.Errorf("alpha host mean = %v, want 0.18", alpha.HostCallMeanSeconds)
	}
	// Merged buckets: obs 0.01 (le=.01), 0.03 (le=.05), 0.5 (le=.5).
	if !approx(alpha.HostCallP50Seconds, 0.0375) {
		t.Errorf("alpha host p50 = %v, want 0.0375", alpha.HostCallP50Seconds)
	}
	if !approx(alpha.HostCallP95Seconds, 0.4625) {
		t.Errorf("alpha host p95 = %v, want 0.4625", alpha.HostCallP95Seconds)
	}
	if alpha.HostCallP99Seconds <= 0.25 || alpha.HostCallP99Seconds > 0.5 {
		t.Errorf("alpha host p99 = %v, want within (0.25, 0.5]", alpha.HostCallP99Seconds)
	}

	// beta: entry-only plugin; host and memory families contribute zeros.
	if beta.EntryCalls != 4 || beta.EntryErrors != 1 {
		t.Errorf("beta entry calls/errors = %d/%d, want 4/1", beta.EntryCalls, beta.EntryErrors)
	}
	if beta.HostCalls != 0 || beta.HostErrors != 0 || beta.CapabilityDenials != 0 || beta.StorageBytes != 0 ||
		beta.MemoryHighWaterBytes != 0 || beta.MemoryLimitExceeded != 0 {
		t.Errorf("beta zero families = %+v", beta)
	}
	if !approx(beta.EntryCallMeanSeconds, 0.101) {
		t.Errorf("beta entry mean = %v, want 0.101", beta.EntryCallMeanSeconds)
	}
	if !approx(beta.EntryCallP95Seconds, 0.235) {
		t.Errorf("beta entry p95 = %v, want 0.235", beta.EntryCallP95Seconds)
	}

	// delta: memory-gauge-only plugin — a row with zeros everywhere else.
	if delta.MemoryHighWaterBytes != 768<<10 {
		t.Errorf("delta memory high-water = %d, want %d", delta.MemoryHighWaterBytes, 768<<10)
	}
	if delta.HostCalls != 0 || delta.EntryCalls != 0 || delta.CapabilityDenials != 0 ||
		delta.StorageBytes != 0 || delta.MemoryLimitExceeded != 0 {
		t.Errorf("delta zero families = %+v", delta)
	}

	// gamma: storage-only plugin — 150 bytes summed over op/scope; memory
	// gauge absent for this plugin.
	if gamma.StorageBytes != 150 {
		t.Errorf("gamma storage bytes = %d, want 150", gamma.StorageBytes)
	}
	if gamma.HostCalls != 0 || gamma.EntryCalls != 0 || gamma.CapabilityDenials != 0 ||
		gamma.HostCallMeanSeconds != 0 || gamma.EntryCallP95Seconds != 0 ||
		gamma.MemoryHighWaterBytes != 0 || gamma.MemoryLimitExceeded != 0 {
		t.Errorf("gamma zero families = %+v", gamma)
	}
}

func TestConsolePluginsNilMetrics(t *testing.T) {
	h, _, _, _ := testHandler()
	h.Metrics = nil
	rr := do(t, adminChain(h), http.MethodGet, "/console/api/plugins")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil metrics registry = %d, want 503", rr.Code)
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q, want JSON", rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(rr.Body.String(), `"error":"metrics disabled"`) {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestJobState(t *testing.T) {
	tests := []struct {
		row  jobs.Row
		want string
	}{
		{row: jobs.Row{}, want: "queued"},
		{row: jobs.Row{StartedAt: 1}, want: "running"},
		{row: jobs.Row{StartedAt: 1, CompletedAt: 2}, want: "done"},
		{row: jobs.Row{LastError: "x"}, want: "failed"},
		{row: jobs.Row{StartedAt: 1, CompletedAt: 2, LastError: "x"}, want: "done"}, // completed wins
	}
	for _, tt := range tests {
		if got := jobState(tt.row); got != tt.want {
			t.Errorf("jobState(%+v) = %q, want %q", tt.row, got, tt.want)
		}
	}
}
