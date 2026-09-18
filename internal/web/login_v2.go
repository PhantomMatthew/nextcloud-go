package web

import (
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/login"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

const wwwAuthenticateValue = `Basic realm="Authorisation Required"`

type AppPasswordIssuer interface {
	Issue(r *http.Request, principal *auth.Principal) (string, error)
}

type LoginV2 struct {
	Service    *login.Service
	Verifier   auth.Verifier
	Issuer     AppPasswordIssuer
	BaseURL    func(*http.Request) string
	FlowRoute  string
	PollRoute  string
	Sessions   session.Store
	Users      users.Store
	SessionTTL time.Duration
	Now        func() time.Time
}

func NewLoginV2(svc *login.Service, verifier auth.Verifier, issuer AppPasswordIssuer) *LoginV2 {
	return &LoginV2{
		Service:   svc,
		Verifier:  verifier,
		Issuer:    issuer,
		BaseURL:   defaultBaseURL,
		FlowRoute: "/index.php/login/v2/flow",
		PollRoute: "/index.php/login/v2/poll",
	}
}

func defaultBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

type initResponse struct {
	Poll  initPoll `json:"poll"`
	Login string   `json:"login"`
}

type initPoll struct {
	Token    string `json:"token"`
	Endpoint string `json:"endpoint"`
}

type pollResponse struct {
	Server      string `json:"server"`
	LoginName   string `json:"loginName"`
	AppPassword string `json:"appPassword"`
}

func (h *LoginV2) HandleInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clientName := r.Header.Get("User-Agent")
	if clientName == "" {
		clientName = "unknown client"
	}
	flow, err := h.Service.Init(r.Context(), clientName)
	if err != nil {
		http.Error(w, "init failed", http.StatusInternalServerError)
		return
	}
	base := h.BaseURL(r)
	resp := initResponse{
		Poll: initPoll{
			Token:    flow.PollToken,
			Endpoint: base + h.PollRoute,
		},
		Login: base + h.FlowRoute + "/" + flow.LoginToken,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *LoginV2) HandlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.FormValue("token")
	if token == "" {
		writeJSONStatus(w, http.StatusNotFound, []any{})
		return
	}
	flow, err := h.Service.Poll(r.Context(), token)
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, []any{})
		return
	}
	resp := pollResponse{
		Server:      flow.Server,
		LoginName:   flow.LoginName,
		AppPassword: flow.AppPassword,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *LoginV2) HandleFlowToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, h.FlowRoute+"/")
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	flow, err := h.Service.BeginGrant(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target := h.FlowRoute + "?stateToken=" + flow.StateToken
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusSeeOther)
}

func (h *LoginV2) HandlePicker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := h.requireAuth(w, r)
	if !ok {
		return
	}
	stateToken := r.URL.Query().Get("stateToken")
	if stateToken == "" {
		http.Error(w, "missing stateToken", http.StatusBadRequest)
		return
	}
	flow, err := h.Service.LookupState(r.Context(), stateToken)
	if err != nil {
		http.Error(w, "invalid or expired flow", http.StatusNotFound)
		return
	}
	page := pickerHTML(principal.UID, flow.ClientName, stateToken)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}

func (h *LoginV2) HandleGrant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := h.requireAuth(w, r)
	if !ok {
		return
	}
	stateToken := r.FormValue("stateToken")
	if stateToken == "" {
		http.Error(w, "missing stateToken", http.StatusBadRequest)
		return
	}
	if _, err := h.Service.LookupState(r.Context(), stateToken); err != nil {
		http.Error(w, "invalid or expired flow", http.StatusNotFound)
		return
	}
	appPassword, err := h.Issuer.Issue(r, principal)
	if err != nil {
		http.Error(w, "failed to issue app password", http.StatusInternalServerError)
		return
	}
	server := h.BaseURL(r)
	if _, err := h.Service.Grant(r.Context(), stateToken, server, principal.UID, appPassword); err != nil {
		http.Error(w, "failed to record grant", http.StatusInternalServerError)
		return
	}
	h.setSessionCookie(w, r, principal)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(grantedHTML()))
}

func (h *LoginV2) requireAuth(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	user, pass, ok := auth.ParseBasicHeader(r.Header.Get("Authorization"))
	if !ok {
		writePlainUnauthorized(w)
		return nil, false
	}
	principal, err := h.Verifier.Verify(r.Context(), user, pass)
	if err != nil {
		writePlainUnauthorized(w)
		return nil, false
	}
	return principal, true
}

func (h *LoginV2) setSessionCookie(w http.ResponseWriter, r *http.Request, principal *auth.Principal) {
	if h.Sessions == nil || h.Users == nil || principal == nil {
		return
	}
	u, err := h.Users.GetByUID(r.Context(), principal.UID)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	ttl := h.SessionTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	ua := r.Header.Get("User-Agent")
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	}
	sess, err := h.Sessions.Create(r.Context(), u.ID, ua, ip, ttl, now)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Phase 1 login v2 is HTTP; Secure follows TLS in a later phase.
		Name:     session.CookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   false,
		MaxAge:   int(ttl.Seconds()),
	})
}

func writePlainUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", wwwAuthenticateValue)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("Authorisation Required\n"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "marshal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, v)
}

func pickerHTML(uid, clientName, stateToken string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Connect to your account</title>
<style>
body { font-family: -apple-system, system-ui, sans-serif; margin: 4rem auto; max-width: 32rem; padding: 0 1rem; }
button { font-size: 1rem; padding: 0.6rem 1.2rem; cursor: pointer; }
.client { font-weight: 600; }
</style>
</head>
<body>
<h1>Connect to your account</h1>
<p>You are signed in as <span class="client">%s</span>.</p>
<p>The application <span class="client">%s</span> is requesting access to your account.</p>
<form method="POST" action="/index.php/login/v2/grant">
<input type="hidden" name="stateToken" value="%s">
<button type="submit">Grant access</button>
</form>
</body>
</html>`,
		html.EscapeString(uid),
		html.EscapeString(clientName),
		html.EscapeString(stateToken),
	)
}

func grantedHTML() string {
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Account connected</title>
<style>
body { font-family: -apple-system, system-ui, sans-serif; margin: 4rem auto; max-width: 32rem; padding: 0 1rem; }
</style>
</head>
<body>
<h1>Account connected</h1>
<p>You can close this window and return to the application.</p>
</body>
</html>`
}
