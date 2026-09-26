package auth

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/session"
)

const DefaultCookie = session.CookieName

// SessionKeyUnlocker opens a session row's sealed private-key copy
// (ADR-0100). *encrypt.SQLResolver satisfies it structurally; auth must not
// import encrypt, so the seam is declared here.
type SessionKeyUnlocker interface {
	UnsealSessionKey(ctx context.Context, sessionID string, sealed []byte) ([]byte, error)
}

// SessionVerifier authenticates nc_session_id cookies.
type SessionVerifier struct {
	Sessions session.Store
	Users    UserSource
	// Keys, when non-nil, opens the session's sealed key copy (SealedUK)
	// and attaches the unlocked private key to the Principal. Nil-ok: no key
	// attach, phase 1–3 behavior.
	Keys SessionKeyUnlocker
}

func (v *SessionVerifier) VerifyID(ctx context.Context, sid string) (*Principal, error) {
	if v == nil || v.Sessions == nil || sid == "" {
		return nil, ErrInvalidCredentials
	}
	sess, err := v.Sessions.Get(ctx, sid)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if v.Users == nil {
		return nil, ErrInvalidCredentials
	}
	u, err := v.Users.GetByID(ctx, sess.UserID)
	if err != nil || !u.Enabled {
		return nil, ErrInvalidCredentials
	}
	p := &Principal{UID: u.UID, DisplayName: u.DisplayName, Enabled: true, AuthMethod: AuthMethodSession}
	if sess.SealedUK != nil && v.Keys != nil {
		// Fail closed: a corrupt key copy must not silently degrade to
		// per-file 403s — the session is rejected instead.
		key, err := v.Keys.UnsealSessionKey(ctx, sid, sess.SealedUK)
		if err != nil {
			return nil, ErrInvalidCredentials
		}
		p.UnlockedKey = key
	}
	touchSession(v.Sessions, ctx, sid)
	return p, nil
}

func touchSession(store session.Store, ctx context.Context, sid string) {
	if store == nil {
		return
	}
	if err := store.Touch(ctx, sid, time.Now().UTC(), 24*time.Hour); err != nil {
		return
	}
}

// MiddlewareConfig wires unified authentication.
type MiddlewareConfig struct {
	Verifier   Verifier
	Bearer     *BearerVerifier
	Sessions   *SessionVerifier
	Throttle   Throttler
	Cookie     string
	Action     string
	OnAuthFail func(http.ResponseWriter, *http.Request, error)
	After      func(time.Duration)
	// RequestToken, when non-nil, arms CSRF validation for
	// session-authenticated unsafe requests (ADR-0064). Requests proven to
	// use basic, app-password, bearer, or public-link token credentials are
	// inherently exempt.
	RequestToken *RequestToken
}

func Middleware(cfg MiddlewareConfig) func(http.Handler) http.Handler {
	if cfg.Cookie == "" {
		cfg.Cookie = DefaultCookie
	}
	if cfg.Action == "" {
		cfg.Action = "login"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := authenticate(r, cfg)
			if err != nil {
				delay := throttleDelay(r, cfg)
				if delay > 0 {
					after := cfg.After
					if after == nil {
						after = time.Sleep
					}
					after(delay)
				}
				if cfg.OnAuthFail != nil {
					cfg.OnAuthFail(w, r, err)
					return
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="Authorisation Required"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !checkRequestToken(r, cfg, p) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("CSRF check failed\n"))
				return
			}
			if p.UnlockedKey != nil {
				// Best-effort zeroing of the session-attached unlocked key
				// (ADR-0100) once the request completes; Go gives no
				// guarantees — noted, accepted in the ADR.
				defer clear(p.UnlockedKey)
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), p)))
		})
	}
}

func authenticate(r *http.Request, cfg MiddlewareConfig) (*Principal, error) {
	ctx := r.Context()
	if cfg.Bearer != nil {
		if tok, ok := parseBearerHeader(r.Header.Get("Authorization")); ok {
			return cfg.Bearer.VerifyToken(ctx, tok)
		}
	}
	if cfg.Verifier != nil {
		user, pass, ok := ParseBasicHeader(r.Header.Get("Authorization"))
		if ok {
			return cfg.Verifier.Verify(ctx, user, pass)
		}
	}
	if cfg.Sessions != nil {
		c, err := r.Cookie(cfg.Cookie)
		if err == nil && c.Value != "" {
			return cfg.Sessions.VerifyID(ctx, c.Value)
		}
	}
	return nil, ErrNoCredentials
}

func throttleDelay(r *http.Request, cfg MiddlewareConfig) time.Duration {
	if cfg.Throttle == nil {
		return 0
	}
	d, err := cfg.Throttle.Observe(r.Context(), clientIP(r), cfg.Action, time.Now().UTC())
	if err != nil {
		return 0
	}
	return d
}

func parseBearerHeader(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// csrfSafeMethod reports whether m never changes server state and is exempt
// from requesttoken validation; PROPFIND/REPORT are read-only WebDAV verbs.
func csrfSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, "PROPFIND", "REPORT":
		return true
	}
	return false
}

// checkRequestToken enforces the CSRF requesttoken once a session cookie has
// authenticated the request. The token echoes, in the requesttoken header or
// form field, the HMAC derived from the very session ID the cookie carried —
// a cross-site attacker cannot know it. A failure is a 403, not a fallthrough
// to 401: the session is valid, the request is not.
func checkRequestToken(r *http.Request, cfg MiddlewareConfig, p *Principal) bool {
	if cfg.RequestToken == nil || p.AuthMethod != AuthMethodSession || csrfSafeMethod(r.Method) {
		return true
	}
	c, err := r.Cookie(cfg.Cookie)
	if err != nil || c.Value == "" {
		return false
	}
	token := r.Header.Get(RequestTokenHeader)
	if token == "" {
		token = formToken(r)
	}
	return cfg.RequestToken.Verify(c.Value, token)
}

// maxFormTokenBytes bounds the form body buffered while looking for a
// requesttoken field; the full stream is restored for the handler either way.
const maxFormTokenBytes = 1 << 20

// formToken extracts the requesttoken from a urlencoded form body. Unlike
// r.FormValue it restores r.Body, because downstream handlers (e.g. the
// sharing OCS endpoint) read the body stream directly.
func formToken(r *http.Request) string {
	if r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxFormTokenBytes))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
	if err != nil {
		return ""
	}
	vals, err := url.ParseQuery(string(b))
	if err != nil {
		return ""
	}
	return vals.Get(RequestTokenHeader)
}
