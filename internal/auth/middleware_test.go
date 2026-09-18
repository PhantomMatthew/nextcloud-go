package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
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
