package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
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

type fakeDBProbe struct {
	dialect database.Dialect
	pingErr error
}

func (f fakeDBProbe) Ping(_ context.Context) error { return f.pingErr }
func (f fakeDBProbe) Dialect() database.Dialect    { return f.dialect }

func testHandler() (*Handler, *fakeUserStore, *fakeJobsStore) {
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
	h := &Handler{
		Users: fu,
		Jobs:  fj,
		DB:    fakeDBProbe{dialect: database.DialectSQLite},
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
	}
	return h, fu, fj
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
	h, _, _ := testHandler()
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
	h, _, _ := testHandler()
	rr := do(t, adminChain(h), http.MethodHead, "/console/console.js")
	if rr.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD body = %d bytes, want 0", rr.Body.Len())
	}
}

func TestConsoleAuthMiddleware(t *testing.T) {
	h, _, _ := testHandler()
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
	h, _, _ := testHandler()
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
	h, fu, _ := testHandler()
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
	h, _, _ := testHandler()
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
	h, _, fj := testHandler()
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
