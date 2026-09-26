package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
)

type memSessions struct {
	byID map[string]*session.Session
}

func (m *memSessions) Create(_ context.Context, userID int64, ua, ip string, ttl time.Duration, now time.Time) (*session.Session, error) {
	id, err := session.NewID()
	if err != nil {
		return nil, err
	}
	s := &session.Session{ID: id, UserID: userID, UserAgent: ua, IP: ip, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(ttl)}
	if m.byID == nil {
		m.byID = map[string]*session.Session{}
	}
	m.byID[id] = s
	return s, nil
}

func (m *memSessions) Get(_ context.Context, id string) (*session.Session, error) {
	s, ok := m.byID[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *memSessions) Touch(_ context.Context, id string, now time.Time, ttl time.Duration) error {
	s, ok := m.byID[id]
	if !ok {
		return session.ErrNotFound
	}
	s.LastSeenAt = now
	s.ExpiresAt = now.Add(ttl)
	return nil
}

func (m *memSessions) Delete(_ context.Context, id string) error {
	if _, ok := m.byID[id]; !ok {
		return session.ErrNotFound
	}
	delete(m.byID, id)
	return nil
}

func (m *memSessions) SetSealedUK(_ context.Context, id string, sealed []byte) error {
	s, ok := m.byID[id]
	if !ok {
		return session.ErrNotFound
	}
	s.SealedUK = sealed
	return nil
}

type memUsers struct {
	byID  map[int64]*UserInfo
	byUID map[string]*UserInfo
}

func (m *memUsers) Create(_ context.Context, u *UserInfo) error {
	if m.byID == nil {
		m.byID = map[int64]*UserInfo{}
		m.byUID = map[string]*UserInfo{}
	}
	cp := *u
	m.byID[u.ID] = &cp
	m.byUID[u.UID] = &cp
	return nil
}

func (m *memUsers) GetByUID(_ context.Context, uid string) (*UserInfo, error) {
	u, ok := m.byUID[uid]
	if !ok {
		return nil, ErrInvalidCredentials
	}
	cp := *u
	return &cp, nil
}

func (m *memUsers) GetByID(_ context.Context, id int64) (*UserInfo, error) {
	u, ok := m.byID[id]
	if !ok {
		return nil, ErrInvalidCredentials
	}
	cp := *u
	return &cp, nil
}

func TestMiddlewareBearerAndBadCookie(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	raw, _, err := IssueAppPassword(ctx, store, "s", "alice", "alice", "cli", TokenTypePermanent)
	if err != nil {
		t.Fatal(err)
	}
	us := &memUsers{}
	_ = us.Create(ctx, &UserInfo{ID: 1, UID: "alice", DisplayName: "Alice", Enabled: true})
	mw := Middleware(MiddlewareConfig{
		Verifier: NewAppPasswordVerifier(store, "s"),
		Bearer:   &BearerVerifier{Store: store, Users: us, Secret: "s"},
		Sessions: &SessionVerifier{Sessions: &memSessions{byID: map[string]*session.Session{}}, Users: us},
		Cookie:   session.CookieName,
		OnAuthFail: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	okH := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := UserFromContext(r.Context())
		if p.UID != "alice" {
			t.Errorf("uid %q", p.UID)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	okH.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer status %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "nope"})
	okH.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad cookie status %d", rr.Code)
	}
}

func TestMiddlewareThrottleAfter(t *testing.T) {
	mem, err := cache.NewMemory(cache.MemoryConfig{MaxItems: 100, MaxCostBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mem.Close)
	var n int
	mw := Middleware(MiddlewareConfig{
		Verifier: stubFailVerifier{},
		Throttle: NewCacheThrottler(mem, 8, 30*time.Second),
		After:    func(time.Duration) { n++ },
		OnAuthFail: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next")
	}))
	for i := 0; i < 9; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Basic YWxpY2U6eA==")
		req.RemoteAddr = "10.0.0.1:1234"
		h.ServeHTTP(rr, req)
	}
	if n == 0 {
		t.Fatal("expected After to run on 9th failure")
	}
}

type stubFailVerifier struct{}

func (stubFailVerifier) Verify(context.Context, string, string) (*Principal, error) {
	return nil, ErrInvalidCredentials
}

type stubPassVerifier struct{}

func (stubPassVerifier) Verify(_ context.Context, user, _ string) (*Principal, error) {
	return &Principal{UID: user, Enabled: true, AuthMethod: AuthMethodBasic}, nil
}

// csrfRig mounts the middleware with session + basic auth and requesttoken
// enforcement over a fixture session with a known ID.
func csrfRig(t *testing.T, next http.Handler) (http.Handler, string) {
	t.Helper()
	ctx := context.Background()
	const sid = "csrf-session-id"
	us := &memUsers{}
	if err := us.Create(ctx, &UserInfo{ID: 1, UID: "alice", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ms := &memSessions{byID: map[string]*session.Session{
		sid: {ID: sid, UserID: 1, ExpiresAt: time.Now().Add(time.Hour)},
	}}
	mw := Middleware(MiddlewareConfig{
		Verifier:     stubPassVerifier{},
		Sessions:     &SessionVerifier{Sessions: ms, Users: us},
		Cookie:       session.CookieName,
		RequestToken: NewRequestToken("csrf-secret"),
		OnAuthFail: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	return mw(next), sid
}

func sessionRequest(t *testing.T, method, sid string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, "/x", body)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: sid})
	return req
}

func TestCSRFSessionUnsafeWithoutTokenIs403(t *testing.T) {
	t.Parallel()
	h, sid := csrfRig(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("next must not run")
	}))
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"missing", ""},
		{"garbage", "not-a-token"},
		{"other session", NewRequestToken("csrf-secret").Derive("other-session")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sessionRequest(t, http.MethodPost, sid, nil)
			if tc.token != "" {
				req.Header.Set(RequestTokenHeader, tc.token)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rr.Code)
			}
		})
	}
}

func TestCSRFSessionUnsafeWithHeaderTokenPasses(t *testing.T) {
	t.Parallel()
	h, sid := csrfRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := sessionRequest(t, http.MethodPost, sid, nil)
	req.Header.Set(RequestTokenHeader, NewRequestToken("csrf-secret").Derive(sid))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestCSRFSessionFormTokenPassesAndBodyIntact(t *testing.T) {
	t.Parallel()
	h, sid := csrfRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("downstream body read: %v", err)
		}
		if !strings.Contains(string(b), "payload=1") {
			t.Errorf("downstream body = %q, want full form", string(b))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	token := NewRequestToken("csrf-secret").Derive(sid)
	form := url.Values{"payload": {"1"}, RequestTokenHeader: {token}}.Encode()
	req := sessionRequest(t, http.MethodPost, sid, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestCSRFSafeMethodsSkipToken(t *testing.T) {
	t.Parallel()
	h, sid := csrfRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, "PROPFIND", "REPORT"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, sessionRequest(t, m, sid, nil))
		if rr.Code != http.StatusNoContent {
			t.Errorf("%s without token = %d, want 204", m, rr.Code)
		}
	}
}

func TestCSRFNonSessionAuthExempt(t *testing.T) {
	t.Parallel()
	h, _ := csrfRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", nil)
	req.Header.Set("Authorization", "Basic YWxpY2U6cHc=")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("basic-auth POST = %d, want 204 (no requesttoken needed)", rr.Code)
	}
}

func TestCSRFDisabledWhenTokenNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	us := &memUsers{}
	if err := us.Create(ctx, &UserInfo{ID: 1, UID: "alice", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ms := &memSessions{byID: map[string]*session.Session{
		"s1": {ID: "s1", UserID: 1, ExpiresAt: time.Now().Add(time.Hour)},
	}}
	h := Middleware(MiddlewareConfig{
		Sessions: &SessionVerifier{Sessions: ms, Users: us},
		Cookie:   session.CookieName,
		OnAuthFail: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, sessionRequest(t, http.MethodPost, "s1", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("session POST with nil RequestToken = %d, want 204", rr.Code)
	}
}

// stubKeyUnlocker is a SessionKeyUnlocker fake: it returns key, or fails
// when the sealed copy is the "corrupt" marker.
type stubKeyUnlocker struct {
	key     []byte
	calls   int
	lastSID string
}

func (s *stubKeyUnlocker) UnsealSessionKey(_ context.Context, sessionID string, sealed []byte) ([]byte, error) {
	s.calls++
	s.lastSID = sessionID
	if string(sealed) == "corrupt" {
		return nil, errors.New("unseal failed")
	}
	out := make([]byte, len(s.key))
	copy(out, s.key)
	return out, nil
}

// TestSessionVerifierKeyAttach pins the ADR-0100 session key attach: a
// session row carrying sealed_uk unlocks through Keys onto the Principal; a
// corrupt copy fails the session closed; no Keys or no copy attaches
// nothing.
func TestSessionVerifierKeyAttach(t *testing.T) {
	ctx := context.Background()
	us := &memUsers{}
	if err := us.Create(ctx, &UserInfo{ID: 1, UID: "alice", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ms := &memSessions{byID: map[string]*session.Session{
		"with-key":  {ID: "with-key", UserID: 1, ExpiresAt: time.Now().Add(time.Hour), SealedUK: []byte("sealed")},
		"no-key":    {ID: "no-key", UserID: 1, ExpiresAt: time.Now().Add(time.Hour)},
		"corrupt":   {ID: "corrupt", UserID: 1, ExpiresAt: time.Now().Add(time.Hour), SealedUK: []byte("corrupt")},
		"stale-key": {ID: "stale-key", UserID: 1, ExpiresAt: time.Now().Add(time.Hour), SealedUK: []byte("sealed")},
	}}
	unlocker := &stubKeyUnlocker{key: []byte("0123456789abcdef0123456789abcdef")}
	v := &SessionVerifier{Sessions: ms, Users: us, Keys: unlocker}

	p, err := v.VerifyID(ctx, "with-key")
	if err != nil {
		t.Fatal(err)
	}
	if string(p.UnlockedKey) != "0123456789abcdef0123456789abcdef" {
		t.Errorf("UnlockedKey = %q", p.UnlockedKey)
	}
	if unlocker.lastSID != "with-key" {
		t.Errorf("unseal saw session %q, want with-key", unlocker.lastSID)
	}

	// No copy on the row: no attach, no unseal call.
	before := unlocker.calls
	p, err = v.VerifyID(ctx, "no-key")
	if err != nil {
		t.Fatal(err)
	}
	if p.UnlockedKey != nil {
		t.Errorf("UnlockedKey = %q, want nil", p.UnlockedKey)
	}
	if unlocker.calls != before {
		t.Error("unseal called for a keyless session row")
	}

	// A corrupt copy fails the session closed (never a silent degrade).
	if _, err := v.VerifyID(ctx, "corrupt"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("corrupt copy err = %v, want ErrInvalidCredentials", err)
	}

	// A copy with no Keys configured is ignored (phase 1–3 behavior).
	vNoKeys := &SessionVerifier{Sessions: ms, Users: us}
	p, err = vNoKeys.VerifyID(ctx, "stale-key")
	if err != nil {
		t.Fatal(err)
	}
	if p.UnlockedKey != nil {
		t.Errorf("UnlockedKey without Keys = %q, want nil", p.UnlockedKey)
	}
}

// TestMiddlewareZeroesUnlockedKey pins the best-effort zeroing: the key the
// verifier attached is cleared once the request completes.
func TestMiddlewareZeroesUnlockedKey(t *testing.T) {
	ctx := context.Background()
	us := &memUsers{}
	if err := us.Create(ctx, &UserInfo{ID: 1, UID: "alice", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ms := &memSessions{byID: map[string]*session.Session{
		"sid": {ID: "sid", UserID: 1, ExpiresAt: time.Now().Add(time.Hour), SealedUK: []byte("sealed")},
	}}
	unlocker := &stubKeyUnlocker{key: []byte("0123456789abcdef0123456789abcdef")}
	mw := Middleware(MiddlewareConfig{
		Sessions: &SessionVerifier{Sessions: ms, Users: us, Keys: unlocker},
		Cookie:   session.CookieName,
		OnAuthFail: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	var attached []byte
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := UserFromContext(r.Context())
		if p.UnlockedKey == nil {
			t.Error("no key attached inside the request")
		}
		attached = p.UnlockedKey
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, sessionRequest(t, http.MethodGet, "sid", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d", rr.Code)
	}
	for i, b := range attached {
		if b != 0 {
			t.Fatalf("UnlockedKey byte %d not zeroed after the request", i)
		}
	}
}
