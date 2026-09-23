package console

import (
	"net/http"
	"slices"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// Auth authenticates console requests with the same credential stack as the
// other browser-facing mounts (session cookie, basic, app-password, bearer).
// It mirrors the webdav.Auth/ocs.Auth pattern — one auth.Middleware with a
// surface-specific failure writer: here the console's own 401 (JSON on the
// api paths, a login-link page elsewhere) instead of an empty WebDAV basic
// challenge an anonymous browser cannot use.
func Auth(cfg auth.MiddlewareConfig) func(http.Handler) http.Handler {
	cfg.Action = "console"
	cfg.OnAuthFail = func(w http.ResponseWriter, r *http.Request, _ error) {
		writeAuthError(w, r, http.StatusUnauthorized, "authentication required")
	}
	return auth.Middleware(cfg)
}

// RequireAdmin gates a handler to members of the admin group — Nextcloud's
// instance-administrators convention (ADR-0080). It runs after Auth: no
// principal is a 401, a principal without the admin group is a 403, and a
// failing group lookup is a 500 (an indeterminate answer must never open
// the gate).
func RequireAdmin(groups GroupLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.UserFromContext(r.Context())
			if !ok || p == nil {
				writeAuthError(w, r, http.StatusUnauthorized, "authentication required")
				return
			}
			gids, err := groups.UserGroupGIDs(r.Context(), p.UID)
			if err != nil {
				writeError(w, r, "group lookup failed")
				return
			}
			if !slices.Contains(gids, users.AdminGroupGID) {
				writeAuthError(w, r, http.StatusForbidden, "administrator access required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
