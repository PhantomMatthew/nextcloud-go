package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

var headTokenPattern = regexp.MustCompile(`data-requesttoken="([^"]+)"`)

type stubUserSource struct{ u *auth.UserInfo }

func (s stubUserSource) GetByUID(_ context.Context, uid string) (*auth.UserInfo, error) {
	if s.u == nil || s.u.UID != uid {
		return nil, auth.ErrInvalidCredentials
	}
	cp := *s.u
	return &cp, nil
}

func (s stubUserSource) GetByID(_ context.Context, id int64) (*auth.UserInfo, error) {
	if s.u == nil || s.u.ID != id {
		return nil, auth.ErrInvalidCredentials
	}
	cp := *s.u
	return &cp, nil
}

// loginRig wires the browser login handlers, the shell-injecting StaticUI,
// and a probe route behind the session-aware auth core — all over a real
// session SQLStore — the way app.mountRoutes does.
type loginRig struct {
	router   *httpx.Router
	tokens   *auth.RequestToken
	sessions *session.SQLStore
	userID   int64
	delays   []time.Duration
}

func newLoginRig(t *testing.T) *loginRig {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(ctx, `INSERT INTO users (uid, display_name, password_hash, enabled, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
		"alice", "Alice", "x", now, now); err != nil {
		t.Fatal(err)
	}
	var userID int64
	if err := db.QueryRow(ctx, `SELECT id FROM users WHERE uid = ?`, "alice").Scan(&userID); err != nil {
		t.Fatal(err)
	}

	tokens := auth.NewRequestToken("rig-secret")
	sessions := session.NewSQLStore(db)
	mem, err := cache.NewMemory(cache.MemoryConfig{MaxItems: 100, MaxCostBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mem.Close)
	rig := &loginRig{tokens: tokens, sessions: sessions, userID: userID}

	bl := &BrowserLogin{
		Verifier: stubVerifier{user: "alice", pass: "wonderland"},
		Users:    stubUserStore{u: &users.User{ID: userID, UID: "alice", DisplayName: "Alice", Enabled: true}},
		Sessions: sessions,
		Tokens:   tokens,
		Throttle: auth.NewCacheThrottler(mem, 1, time.Minute),
		After:    func(d time.Duration) { rig.delays = append(rig.delays, d) },
	}

	ui := newStaticFixture(t)
	ui.Shell = &BrowserBootstrap{Sessions: sessions, Tokens: tokens}

	src := stubUserSource{u: &auth.UserInfo{ID: userID, UID: "alice", DisplayName: "Alice", Enabled: true}}
	probe := auth.Middleware(auth.MiddlewareConfig{
		Sessions:     &auth.SessionVerifier{Sessions: sessions, Users: src},
		Verifier:     stubVerifier{user: "alice", pass: "wonderland"},
		Cookie:       session.CookieName,
		RequestToken: tokens,
		OnAuthFail: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			// Read the body so tests can prove the form-token fallback did
			// not consume it.
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	router := httpx.NewRouter(httpx.CSRF(httpx.CSRFConfig{
		// Mirrors app.mountRoutes: login/logout self-validate, session-cookie
		// requests defer to the auth core's requesttoken check.
		PathBypass:    []string{"/index.php/login", "/index.php/logout"},
		SessionCookie: session.CookieName,
	}))
	router.Handle(http.MethodPost, "/index.php/login", http.HandlerFunc(bl.HandleLogin))
	router.Handle(http.MethodGet, "/index.php/logout", http.HandlerFunc(bl.HandleLogout))
	router.Handle(http.MethodPost, "/index.php/logout", http.HandlerFunc(bl.HandleLogout))
	router.Handle(http.MethodGet, "/index.php/login", ui)
	router.HandlePrefix(httpx.MethodAny, "/protected/", probe)
	router.HandlePrefix(http.MethodGet, "/", ui)
	router.HandlePrefix(http.MethodHead, "/", ui)
	rig.router = router
	return rig
}

func (rig *loginRig) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	rig.router.ServeHTTP(rr, req)
	return rr
}

func cookieValue(t *testing.T, rr *httptest.ResponseRecorder, name string) string {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	t.Fatalf("cookie %q not set; got %v", name, rr.Result().Cookies())
	return ""
}

func extractHeadToken(t *testing.T, body string) string {
	t.Helper()
	m := headTokenPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no data-requesttoken in shell: %q", body)
	}
	return m[1]
}

func getShell(t *testing.T, rig *loginRig, cookies ...*http.Cookie) (string, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := rig.do(t, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rr.Code)
	}
	return rr.Body.String(), rr
}

func postLogin(t *testing.T, rig *loginRig, nonceCookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if nonceCookie != nil {
		req.AddCookie(nonceCookie)
	}
	return rig.do(t, req)
}

func TestBrowserLoginFullChain(t *testing.T) {
	rig := newLoginRig(t)

	// Anonymous shell: login nonce cookie issued, login token injected in
	// both bootstrap spots.
	body, rr := getShell(t, rig)
	nonce := &http.Cookie{Name: LoginNonceCookie, Value: cookieValue(t, rr, LoginNonceCookie)}
	loginToken := extractHeadToken(t, body)
	if !strings.Contains(body, `window.oc_requesttoken="`+loginToken+`"`) {
		t.Fatalf("shell missing window.oc_requesttoken: %q", body)
	}
	if !rig.tokens.VerifyLoginToken(nonce.Value, loginToken) {
		t.Fatal("injected token must be the login token for the issued nonce")
	}

	// Wrong login token -> 403, no session.
	form := url.Values{"user": {"alice"}, "password": {"wonderland"}, "requesttoken": {"bogus"}}
	if rr := postLogin(t, rig, nonce, form); rr.Code != http.StatusForbidden {
		t.Fatalf("bad token login = %d, want 403", rr.Code)
	}

	// Right token, wrong password -> 401, throttled from the second failure.
	form.Set("requesttoken", loginToken)
	form.Set("password", "wrong")
	for i := 0; i < 2; i++ {
		if rr := postLogin(t, rig, nonce, form); rr.Code != http.StatusUnauthorized {
			t.Fatalf("bad password login %d = %d, want 401", i, rr.Code)
		}
	}
	if len(rig.delays) != 1 {
		t.Fatalf("throttle delays = %v, want exactly one (limit 1)", rig.delays)
	}

	// Right token and password -> 303, session cookie, nonce rotated away.
	form.Set("password", "wonderland")
	rr = postLogin(t, rig, nonce, form)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Fatalf("login Location = %q, want /", loc)
	}
	sess := &http.Cookie{Name: session.CookieName, Value: cookieValue(t, rr, session.CookieName)}
	if c := rr.Result().Cookies(); !cookieExpired(c, LoginNonceCookie) {
		t.Fatal("login must rotate (expire) the anonymous nonce cookie")
	}

	// Authenticated shell now carries the session-derived token.
	body, rr = getShell(t, rig, sess)
	sessToken := extractHeadToken(t, body)
	if want := rig.tokens.Derive(sess.Value); sessToken != want {
		t.Fatalf("authed shell token = %q, want session token %q", sessToken, want)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == LoginNonceCookie {
			t.Fatal("authenticated shell must not re-issue the login nonce")
		}
	}

	// Session POST without a token -> 403 from the auth core.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/protected/thing", nil)
	req.AddCookie(sess)
	if rr := rig.do(t, req); rr.Code != http.StatusForbidden {
		t.Fatalf("session POST without token = %d, want 403", rr.Code)
	}

	// Session POST with the token in the header -> pass.
	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/protected/thing", nil)
	req.AddCookie(sess)
	req.Header.Set(auth.RequestTokenHeader, sessToken)
	if rr := rig.do(t, req); rr.Code != http.StatusNoContent {
		t.Fatalf("session POST with header token = %d, want 204", rr.Code)
	}

	// Token as a form field also satisfies the check.
	formBody := url.Values{auth.RequestTokenHeader: {sessToken}, "x": {"1"}}.Encode()
	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/protected/thing", strings.NewReader(formBody))
	req.AddCookie(sess)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rr := rig.do(t, req); rr.Code != http.StatusNoContent {
		t.Fatalf("session POST with form token = %d, want 204", rr.Code)
	}

	// Basic-auth POST is exempt from requesttoken validation. Real OCS
	// clients also send OCS-APIRequest, which is how they pass the base
	// chain's anonymous blanket without a session cookie.
	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/protected/thing", nil)
	req.Header.Set("Authorization", basicAuth("alice", "wonderland"))
	req.Header.Set("OCS-APIRequest", "true")
	if rr := rig.do(t, req); rr.Code != http.StatusNoContent {
		t.Fatalf("basic POST = %d, want 204", rr.Code)
	}

	// Logout with the token in the query (upstream's logout link shape).
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/index.php/logout?requesttoken="+url.QueryEscape(sessToken), nil)
	req.AddCookie(sess)
	rr = rig.do(t, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/index.php/login" {
		t.Fatalf("logout Location = %q", loc)
	}
	if _, err := rig.sessions.Get(context.Background(), sess.Value); err == nil {
		t.Fatal("logout must delete the session")
	}

	// The dead session cookie no longer authenticates.
	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/protected/thing", nil)
	req.AddCookie(sess)
	req.Header.Set(auth.RequestTokenHeader, sessToken)
	if rr := rig.do(t, req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("POST with deleted session = %d, want 401", rr.Code)
	}
}

func cookieExpired(cookies []*http.Cookie, name string) bool {
	for _, c := range cookies {
		if c.Name == name && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

func TestBrowserLoginRejectsBadRequests(t *testing.T) {
	rig := newLoginRig(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/index.php/login", nil)
	if rr := rig.do(t, req); rr.Code != http.StatusOK {
		// GET /index.php/login is extensionless: the SPA fallback owns it and
		// serves the shell carrying a fresh login token.
		t.Fatalf("GET /index.php/login = %d, want SPA shell 200", rr.Code)
	} else if extractHeadToken(t, rr.Body.String()) == "" {
		t.Fatal("GET /index.php/login shell must carry the login token")
	}

	// No nonce cookie at all -> 403 even with a well-formed token.
	form := url.Values{
		"user": {"alice"}, "password": {"wonderland"},
		"requesttoken": {rig.tokens.DeriveLoginToken("never-issued")},
	}
	if rr := postLogin(t, rig, nil, form); rr.Code != http.StatusForbidden {
		t.Fatalf("login without nonce cookie = %d, want 403", rr.Code)
	}
}

func TestBrowserLogoutTokenEnforced(t *testing.T) {
	rig := newLoginRig(t)
	ctx := context.Background()
	sess, err := rig.sessions.Create(ctx, rig.userID, "ua", "127.0.0.1", time.Hour, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: session.CookieName, Value: sess.ID}

	// Wrong token -> 403, session survives.
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/logout?requesttoken=bogus", nil)
	req.AddCookie(cookie)
	if rr := rig.do(t, req); rr.Code != http.StatusForbidden {
		t.Fatalf("logout with bad token = %d, want 403", rr.Code)
	}
	if _, err := rig.sessions.Get(ctx, sess.ID); err != nil {
		t.Fatal("session must survive a rejected logout")
	}

	// Header token works too (POST shape).
	req = httptest.NewRequestWithContext(ctx, http.MethodPost, "/index.php/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set(auth.RequestTokenHeader, rig.tokens.Derive(sess.ID))
	if rr := rig.do(t, req); rr.Code != http.StatusSeeOther {
		t.Fatalf("logout with header token = %d, want 303", rr.Code)
	}

	// No session cookie -> idempotent 303, no token required.
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/logout", nil)
	if rr := rig.do(t, req); rr.Code != http.StatusSeeOther {
		t.Fatalf("anonymous logout = %d, want 303", rr.Code)
	}
}
