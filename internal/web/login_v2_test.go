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

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/login"
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

func basicAuth(user, pass string) string {
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
	req := httptest.NewRequest(http.MethodPost, "/index.php/login/v2", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/index.php/login/v2", nil)
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
	req := httptest.NewRequest(http.MethodPost, "/index.php/login/v2/poll", body)
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
	req := httptest.NewRequest(http.MethodPost, "/index.php/login/v2/poll", body)
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
	req2 := httptest.NewRequest(http.MethodPost, "/index.php/login/v2/poll", body2)
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
	req := httptest.NewRequest(http.MethodGet, h.FlowRoute+"/"+flow.LoginToken, nil)
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
	req := httptest.NewRequest(http.MethodGet, h.FlowRoute+"/doesnotexist", nil)
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
	req := httptest.NewRequest(http.MethodGet, h.FlowRoute+"?stateToken="+st.StateToken, nil)
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
	req := httptest.NewRequest(http.MethodGet, h.FlowRoute+"?stateToken="+st.StateToken, nil)
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
	req := httptest.NewRequest(http.MethodGet, h.FlowRoute, nil)
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
	req := httptest.NewRequest(http.MethodPost, "/index.php/login/v2/grant", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", basicAuth("alice", "wonderland"))
	w := httptest.NewRecorder()
	h.HandleGrant(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}

	pollBody := strings.NewReader(url.Values{"token": {flow.PollToken}}.Encode())
	pollReq := httptest.NewRequest(http.MethodPost, "/index.php/login/v2/poll", pollBody)
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
	req := httptest.NewRequest(http.MethodPost, "/index.php/login/v2/grant", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.HandleGrant(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
}
