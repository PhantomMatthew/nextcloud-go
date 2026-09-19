package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/login"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

type stubVerifier struct {
	user string
	pass string
}

func (s stubVerifier) Verify(_ context.Context, user, pass string) (*auth.Principal, error) {
	if user != s.user || pass != s.pass {
		return nil, auth.ErrInvalidCredentials
	}
	return &auth.Principal{UID: user, DisplayName: user, Enabled: true, AuthMethod: auth.AuthMethodBasic}, nil
}

type stubIssuer struct {
	password string
	err      error
}

func (s stubIssuer) Issue(_ *http.Request, _ *auth.Principal) (string, error) {
	return s.password, s.err
}

func newHandler(t *testing.T, issuer AppPasswordIssuer) *LoginV2 {
	t.Helper()
	svc := login.NewService(login.NewMemoryStore())
	v := stubVerifier{user: "alice", pass: "wonderland"}
	h := NewLoginV2(svc, v, issuer)
	h.BaseURL = func(_ *http.Request) string { return "https://cloud.example.test" }
	return h
}

func basicAuth(user, pass string) string { //nolint:unparam
	return "Basic " + basicEncode(user+":"+pass)
}

func basicEncode(s string) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	src := []byte(s)
	var dst strings.Builder
	for i := 0; i < len(src); i += 3 {
		var b [3]byte
		n := copy(b[:], src[i:])
		v := uint(b[0])<<16 | uint(b[1])<<8 | uint(b[2])
		dst.WriteByte(tbl[(v>>18)&0x3F])
		dst.WriteByte(tbl[(v>>12)&0x3F])
		if n >= 2 {
			dst.WriteByte(tbl[(v>>6)&0x3F])
		} else {
			dst.WriteByte('=')
		}
		if n >= 3 {
			dst.WriteByte(tbl[v&0x3F])
		} else {
			dst.WriteByte('=')
		}
	}
	return dst.String()
}

func TestHandleInit(t *testing.T) {
	h := newHandler(t, stubIssuer{password: "ignored"})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 nctest")
	w := httptest.NewRecorder()
	h.HandleInit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	var got initResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Poll.Token) != login.TokenLength {
		t.Fatalf("poll token len: got %d want %d", len(got.Poll.Token), login.TokenLength)
	}
	wantPoll := "https://cloud.example.test/index.php/login/v2/poll"
	if got.Poll.Endpoint != wantPoll {
		t.Fatalf("poll endpoint: got %q want %q", got.Poll.Endpoint, wantPoll)
	}
	if !strings.HasPrefix(got.Login, "https://cloud.example.test/index.php/login/v2/flow/") {
		t.Fatalf("login url: %q", got.Login)
	}
}

func TestHandleInitMethodNotAllowed(t *testing.T) {
	h := newHandler(t, stubIssuer{})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/index.php/login/v2", nil)
	w := httptest.NewRecorder()
	h.HandleInit(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", w.Code)
	}
	if a := w.Header().Get("Allow"); a != http.MethodPost {
		t.Fatalf("allow: %q", a)
	}
}

func TestHandlePollPending(t *testing.T) {
	h := newHandler(t, stubIssuer{})
	flow, err := h.Service.Init(context.Background(), "test client")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	body := strings.NewReader(url.Values{"token": {flow.PollToken}}.Encode())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/poll", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.HandlePoll(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("pending status: got %d want 404", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("pending body: %q", w.Body.String())
	}
}

func TestHandlePollGranted(t *testing.T) {
	h := newHandler(t, stubIssuer{password: "app-pw-XYZ"})
	flow, err := h.Service.Init(context.Background(), "test client")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	st, err := h.Service.BeginGrant(context.Background(), flow.LoginToken)
	if err != nil {
		t.Fatalf("begin grant: %v", err)
	}
	if _, err := h.Service.Grant(context.Background(), st.StateToken, "https://cloud.example.test", "alice", "app-pw-XYZ"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	body := strings.NewReader(url.Values{"token": {flow.PollToken}}.Encode())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/poll", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.HandlePoll(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("granted status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var got pollResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LoginName != "alice" || got.AppPassword != "app-pw-XYZ" || got.Server != "https://cloud.example.test" {
		t.Fatalf("poll response: %+v", got)
	}

	body2 := strings.NewReader(url.Values{"token": {flow.PollToken}}.Encode())
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/poll", body2)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	h.HandlePoll(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("consumed status: got %d want 404", w2.Code)
	}
}

func TestHandleFlowTokenRedirect(t *testing.T) {
	h := newHandler(t, stubIssuer{})
	flow, err := h.Service.Init(context.Background(), "test client")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, h.FlowRoute+"/"+flow.LoginToken, nil)
	w := httptest.NewRecorder()
	h.HandleFlowToken(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, h.FlowRoute+"?stateToken=") {
		t.Fatalf("location: %q", loc)
	}
}

func TestHandleFlowTokenUnknown(t *testing.T) {
	h := newHandler(t, stubIssuer{})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, h.FlowRoute+"/doesnotexist", nil)
	w := httptest.NewRecorder()
	h.HandleFlowToken(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", w.Code)
	}
}

func TestHandlePickerRequiresAuth(t *testing.T) {
	h := newHandler(t, stubIssuer{})
	flow, err := h.Service.Init(context.Background(), "test client")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	st, err := h.Service.BeginGrant(context.Background(), flow.LoginToken)
	if err != nil {
		t.Fatalf("begin grant: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, h.FlowRoute+"?stateToken="+st.StateToken, nil)
	w := httptest.NewRecorder()
	h.HandlePicker(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	if a := w.Header().Get("WWW-Authenticate"); a != wwwAuthenticateValue {
		t.Fatalf("www-authenticate: %q", a)
	}
}

func TestHandlePickerAuthorized(t *testing.T) {
	h := newHandler(t, stubIssuer{})
	flow, err := h.Service.Init(context.Background(), "test client")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	st, err := h.Service.BeginGrant(context.Background(), flow.LoginToken)
	if err != nil {
		t.Fatalf("begin grant: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, h.FlowRoute+"?stateToken="+st.StateToken, nil)
	req.Header.Set("Authorization", basicAuth("alice", "wonderland"))
	w := httptest.NewRecorder()
	h.HandlePicker(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: %q", ct)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), st.StateToken) {
		t.Fatalf("body missing state token")
	}
	if !strings.Contains(string(body), "test client") {
		t.Fatalf("body missing client name")
	}
}

func TestHandlePickerMissingStateToken(t *testing.T) {
	h := newHandler(t, stubIssuer{})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, h.FlowRoute, nil)
	req.Header.Set("Authorization", basicAuth("alice", "wonderland"))
	w := httptest.NewRecorder()
	h.HandlePicker(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleGrantSuccess(t *testing.T) {
	h := newHandler(t, stubIssuer{password: "app-pw-grant"})
	flow, err := h.Service.Init(context.Background(), "test client")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	st, err := h.Service.BeginGrant(context.Background(), flow.LoginToken)
	if err != nil {
		t.Fatalf("begin grant: %v", err)
	}
	body := strings.NewReader(url.Values{"stateToken": {st.StateToken}}.Encode())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/grant", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", basicAuth("alice", "wonderland"))
	w := httptest.NewRecorder()
	h.HandleGrant(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}

	pollBody := strings.NewReader(url.Values{"token": {flow.PollToken}}.Encode())
	pollReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/poll", pollBody)
	pollReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pw := httptest.NewRecorder()
	h.HandlePoll(pw, pollReq)
	if pw.Code != http.StatusOK {
		t.Fatalf("poll status: got %d want 200, body=%s", pw.Code, pw.Body.String())
	}
	var got pollResponse
	if err := json.Unmarshal(pw.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LoginName != "alice" || got.AppPassword != "app-pw-grant" {
		t.Fatalf("poll response: %+v", got)
	}
}

type stubUserStore struct {
	u *users.User
}

func (s stubUserStore) Create(context.Context, *users.User) error { return nil }
func (s stubUserStore) GetByUID(_ context.Context, uid string) (*users.User, error) {
	if s.u == nil || s.u.UID != uid {
		return nil, users.ErrNotFound
	}
	cp := *s.u
	return &cp, nil
}

func (s stubUserStore) GetByID(_ context.Context, id int64) (*users.User, error) {
	if s.u == nil || s.u.ID != id {
		return nil, users.ErrNotFound
	}
	cp := *s.u
	return &cp, nil
}
func (stubUserStore) UpdatePasswordHash(context.Context, int64, string) error { return nil }
func (stubUserStore) Count(context.Context) (int64, error)                    { return 1, nil }
func (stubUserStore) CreateGroup(context.Context, *users.Group) error         { return nil }
func (stubUserStore) GetGroupByGID(context.Context, string) (*users.Group, error) {
	return nil, users.ErrNotFound
}
func (stubUserStore) AddGroupMember(context.Context, string, string) error { return nil }
func (stubUserStore) UserGroupGIDs(context.Context, string) ([]string, error) {
	return nil, nil
}

type stubSessionStore struct {
	created *session.Session
}

func (s *stubSessionStore) Create(_ context.Context, userID int64, ua, ip string, ttl time.Duration, now time.Time) (*session.Session, error) {
	s.created = &session.Session{ID: "sessidhex", UserID: userID, UserAgent: ua, IP: ip, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(ttl)}
	return s.created, nil
}

func (*stubSessionStore) Get(context.Context, string) (*session.Session, error) {
	return nil, session.ErrNotFound
}

func (*stubSessionStore) Touch(context.Context, string, time.Time, time.Duration) error {
	return nil
}
func (*stubSessionStore) Delete(context.Context, string) error { return nil }

func TestHandleGrantSetsSessionCookie(t *testing.T) {
	h := newHandler(t, stubIssuer{password: "app-pw-grant"})
	h.Users = stubUserStore{u: &users.User{ID: 7, UID: "alice", DisplayName: "Alice", Enabled: true}}
	ss := &stubSessionStore{}
	h.Sessions = ss
	flow, err := h.Service.Init(context.Background(), "test client")
	if err != nil {
		t.Fatal(err)
	}
	st, err := h.Service.BeginGrant(context.Background(), flow.LoginToken)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(url.Values{"stateToken": {st.StateToken}}.Encode())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/grant", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", basicAuth("alice", "wonderland"))
	w := httptest.NewRecorder()
	h.HandleGrant(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	cookie := w.Result().Cookies()
	found := false
	for _, c := range cookie {
		if c.Name == session.CookieName && c.Value == "sessidhex" && c.HttpOnly {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing session cookie: %+v", cookie)
	}
	if ss.created == nil || ss.created.UserID != 7 {
		t.Fatalf("session %+v", ss.created)
	}
}

func TestHandleGrantUnauthorized(t *testing.T) {
	h := newHandler(t, stubIssuer{password: "x"})
	flow, err := h.Service.Init(context.Background(), "test client")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	st, err := h.Service.BeginGrant(context.Background(), flow.LoginToken)
	if err != nil {
		t.Fatalf("begin grant: %v", err)
	}
	body := strings.NewReader(url.Values{"stateToken": {st.StateToken}}.Encode())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/grant", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.HandleGrant(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
}
