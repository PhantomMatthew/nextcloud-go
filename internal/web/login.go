package web

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// LoginNonceCookie carries the anonymous nonce the login-page requesttoken is
// bound to (double-submit; ADR-0064). It is rotated away at successful login.
const LoginNonceCookie = "ncgo_login_nonce"

// BrowserLogin serves the SPA's browser form login and logout:
// POST /index.php/login and GET|POST /index.php/logout.
type BrowserLogin struct {
	Verifier   auth.Verifier
	Users      users.Store
	Sessions   session.Store
	Tokens     *auth.RequestToken
	Throttle   auth.Throttler
	SessionTTL time.Duration
	Now        func() time.Time
	After      func(time.Duration)
}

// HandleLogin authenticates a browser form POST. The requesttoken field is
// validated against the anonymous nonce cookie first — at this point there is
// no session yet, so the auth middleware cannot do it. A bad token is a 403;
// bad credentials are a 401 recorded with the brute-force throttler; success
// creates a session exactly like a login v2 grant and redirects to the app.
func (h *BrowserLogin) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Verifier == nil || h.Users == nil || h.Sessions == nil || h.Tokens == nil {
		http.Error(w, "login not configured", http.StatusInternalServerError)
		return
	}
	nonce := ""
	if c, err := r.Cookie(LoginNonceCookie); err == nil {
		nonce = c.Value
	}
	if !h.Tokens.VerifyLoginToken(nonce, r.FormValue(auth.RequestTokenHeader)) {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	principal, err := h.Verifier.Verify(r.Context(), r.FormValue("user"), r.FormValue("password"))
	if err != nil {
		h.throttle(r)
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	u, err := h.Users.GetByUID(r.Context(), principal.UID)
	if err != nil {
		http.Error(w, "user lookup failed", http.StatusInternalServerError)
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
		http.Error(w, "session create failed", http.StatusInternalServerError)
		return
	}
	// Rotate the anonymous nonce away: the login token must not be reusable,
	// and the authenticated shell now derives its token from the session.
	expireCookie(w, r, LoginNonceCookie)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme (TLS or X-Forwarded-Proto); serve --dev is plaintext HTTP, where a forced-Secure cookie is dropped. HttpOnly and SameSite=Lax are always set.
		Name:     session.CookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
		MaxAge:   int(ttl.Seconds()),
	})
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusSeeOther)
}

// HandleLogout invalidates the session named by the cookie and returns to the
// login page. The requesttoken (header, else query — upstream's logout link
// is a plain GET) must match that session, so a cross-site request cannot
// force-logout. Without a session cookie there is nothing to protect:
// redirect idempotently.
func (h *BrowserLogin) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := r.Cookie(session.CookieName)
	if err == nil && c.Value != "" {
		token := r.Header.Get(auth.RequestTokenHeader)
		if token == "" {
			token = r.URL.Query().Get(auth.RequestTokenHeader)
		}
		if h.Tokens == nil || !h.Tokens.Verify(c.Value, token) {
			http.Error(w, "CSRF check failed", http.StatusForbidden)
			return
		}
		if h.Sessions != nil {
			// An already-expired row is as good as deleted.
			if err := h.Sessions.Delete(r.Context(), c.Value); err != nil && !errors.Is(err, session.ErrNotFound) {
				http.Error(w, "logout failed", http.StatusInternalServerError)
				return
			}
		}
	}
	expireCookie(w, r, session.CookieName)
	w.Header().Set("Location", "/index.php/login")
	w.WriteHeader(http.StatusSeeOther)
}

func (h *BrowserLogin) throttle(r *http.Request) {
	if h.Throttle == nil {
		return
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	}
	delay, err := h.Throttle.Observe(r.Context(), ip, "login", now)
	if err != nil || delay <= 0 {
		return
	}
	after := h.After
	if after == nil {
		after = time.Sleep
	}
	after(delay)
}

func expireCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme; see HandleLogin. An expiring cookie over plaintext dev HTTP must not be marked Secure or the browser drops it.
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
		MaxAge:   -1,
	})
}

// isSecureRequest reports whether the request arrived over TLS (directly or
// per trusted proxy header), deciding the Secure flag on cookies we set.
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// BrowserBootstrap resolves the requesttoken the SPA shell is injected with
// (StaticUI.Shell): the session token when the request carries a valid
// session, else a token bound to the anonymous login nonce, issuing that
// nonce cookie when missing.
type BrowserBootstrap struct {
	Sessions session.Store
	Tokens   *auth.RequestToken
}

func (b *BrowserBootstrap) RequestToken(w http.ResponseWriter, r *http.Request) string {
	if b == nil || b.Tokens == nil {
		return ""
	}
	if b.Sessions != nil {
		if c, err := r.Cookie(session.CookieName); err == nil && c.Value != "" {
			if _, err := b.Sessions.Get(r.Context(), c.Value); err == nil {
				return b.Tokens.Derive(c.Value)
			}
		}
	}
	nonce := ""
	if c, err := r.Cookie(LoginNonceCookie); err == nil {
		nonce = c.Value
	}
	if nonce == "" {
		n, err := auth.NewLoginNonce()
		if err != nil {
			return ""
		}
		nonce = n
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme; see HandleLogin.
			Name:     LoginNonceCookie,
			Value:    nonce,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   isSecureRequest(r),
		})
	}
	return b.Tokens.DeriveLoginToken(nonce)
}
