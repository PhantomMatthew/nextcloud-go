package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/version"
)

// LoginNonceCookie carries the anonymous nonce the login-page requesttoken is
// bound to (double-submit; ADR-0064). It is rotated away at successful login.
const LoginNonceCookie = "ncgo_login_nonce"

// LoginKeyHandler is the password-wrapped-keys seam of the login handlers
// (ADR-0100, extended by ADR-0102): *encrypt.SQLResolver satisfies it
// structurally; web must not import encrypt. Nil-ok on both handlers.
type LoginKeyHandler interface {
	// UnlockForLogin runs the enrollment state machine at a password login
	// and returns the user's unlocked private key (nil when the user is
	// master-wrapped).
	UnlockForLogin(ctx context.Context, uid, password string) ([]byte, error)
	// SealSessionKey seals an unlocked private key for storage on a session
	// row.
	SealSessionKey(priv []byte, sessionID string) ([]byte, error)
	// WrapKeyForToken seals an unlocked private key under a newly issued app
	// password's raw token (ADR-0102 wrap-at-issuance), so the token's
	// requests can unlock files.
	WrapKeyForToken(ctx context.Context, appPasswordID, tokenRaw string, priv []byte) error
}

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
	// Keys, when non-nil, enrolls/unlocks password-wrapped keys at basic
	// logins and stores the sealed key copy on the new session (ADR-0100);
	// a verifier-attached key (app-password login whose token holds a wrap,
	// ADR-0102) is sealed onto the session the same way.
	Keys LoginKeyHandler
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
	// Enroll/unlock password-wrapped keys (ADR-0100) — only a real password
	// login (basic) ever holds the password; an app password presented at
	// the form neither enrolls nor unlocks. A failure fails the login
	// loudly: silent fallback would orphan every enrolled file.
	var priv []byte
	if principal.AuthMethod == auth.AuthMethodBasic && h.Keys != nil {
		priv, err = h.Keys.UnlockForLogin(r.Context(), principal.UID, r.FormValue("password"))
		if err != nil {
			http.Error(w, "login key unlock failed", http.StatusInternalServerError)
			return
		}
	}
	// ADR-0102: an app-password form login whose token holds a key wrap
	// arrives with the unlocked key attached by the chain verifier — the new
	// session inherits it, sealed onto the session row exactly like a
	// password-unlocked key. (Both key sources exist only when key handling
	// is wired, so priv implies h.Keys non-nil.)
	if priv == nil {
		priv = principal.UnlockedKey
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
		ttl = defaultSessionTTL
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
	if priv != nil {
		// A session without its key copy would 403 every enrolled file —
		// seal/store errors delete the session and fail the login loudly.
		sealed, serr := h.Keys.SealSessionKey(priv, sess.ID)
		if serr == nil {
			serr = h.Sessions.SetSealedUK(r.Context(), sess.ID, sealed)
		}
		if serr != nil {
			//nolint:errcheck // best-effort cleanup of the orphaned session before the 500; a leftover expires naturally
			_ = h.Sessions.Delete(r.Context(), sess.ID)
			http.Error(w, "session key store failed", http.StatusInternalServerError)
			return
		}
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

// defaultSessionTTL is the browser session lifetime the login handlers and
// the session store fall back to; the bootstrap state echoes it to the
// frontend as oc_config.session_lifetime.
const defaultSessionTTL = 24 * time.Hour

// BrowserBootstrap resolves the per-request values the SPA shell is injected
// with (StaticUI.Shell): the session requesttoken plus session-personalized
// bootstrap state when the request carries a valid session, else a token
// bound to the anonymous login nonce (issuing that nonce cookie when
// missing) plus the anonymous public state (ADR-0064, ADR-0069).
type BrowserBootstrap struct {
	Sessions session.Store
	Tokens   *auth.RequestToken
	// Users resolves the session user's uid/displayName for the head
	// data-user attributes; nil degrades to the anonymous key set.
	Users users.Store
	// Webroot is the path prefix the UI is served under ("" = site root,
	// the only layout ncgo mounts).
	Webroot string
	// SessionTTL feeds oc_config.session_lifetime; <=0 defaults to
	// defaultSessionTTL, mirroring the login handlers.
	SessionTTL time.Duration
}

func (b *BrowserBootstrap) Bootstrap(w http.ResponseWriter, r *http.Request) (string, BootstrapState) {
	state := BootstrapState{
		Version:           version.String(),
		VersionString:     version.VersionString,
		ModRewriteWorking: true,
		SessionKeepalive:  true,
		SessionLifetime:   int64(defaultSessionTTL / time.Second),
	}
	if b == nil {
		return "", state
	}
	state.Webroot = b.Webroot
	if b.SessionTTL > 0 {
		state.SessionLifetime = int64(b.SessionTTL / time.Second)
	}
	if b.Tokens == nil {
		return "", state
	}
	if b.Sessions != nil {
		if c, err := r.Cookie(session.CookieName); err == nil && c.Value != "" {
			if sess, err := b.Sessions.Get(r.Context(), c.Value); err == nil {
				state.User = b.bootstrapUser(r.Context(), sess.UserID)
				return b.Tokens.Derive(c.Value), state
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
			return "", state
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
	return b.Tokens.DeriveLoginToken(nonce), state
}

// bootstrapUser resolves the session user for head-attribute injection,
// mirroring the auth middleware's session validity criteria: the user row
// must exist and be enabled, else the shell stays anonymous-shaped.
func (b *BrowserBootstrap) bootstrapUser(ctx context.Context, id int64) *BootstrapUser {
	if b.Users == nil {
		return nil
	}
	u, err := b.Users.GetByID(ctx, id)
	if err != nil || !u.Enabled {
		return nil
	}
	return &BootstrapUser{UID: u.UID, DisplayName: u.DisplayName}
}
