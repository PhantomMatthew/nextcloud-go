package auth

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/session"
)

const DefaultCookie = session.CookieName

// SessionVerifier authenticates nc_session_id cookies.
type SessionVerifier struct {
	Sessions session.Store
	Users    UserSource
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
