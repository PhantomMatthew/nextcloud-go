package httpx

import (
	"net/http"
	"strings"
)

const (
	HeaderOCSAPIRequest = "OCS-APIRequest"
	HeaderAuthorization = "Authorization"

	ocsAPIRequestValue = "true"
	bearerPrefix       = "Bearer "
)

// IsOCSRequest reports whether the request carries the exact
// OCS-APIRequest: true header upstream Nextcloud uses to opt out of
// browser-style CSRF protection.
func IsOCSRequest(r *http.Request) bool {
	return r.Header.Get(HeaderOCSAPIRequest) == ocsAPIRequestValue
}

// IsBearerRequest reports whether the request authenticates via a
// Bearer token (OAuth2 / app password), which also bypasses CSRF.
func IsBearerRequest(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get(HeaderAuthorization), bearerPrefix)
}

// IsSafeMethod reports whether the HTTP method is considered side-effect
// free and therefore exempt from CSRF checks.
func IsSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// CSRFConfig configures the CSRF middleware. When Validate is nil the
// middleware only enforces the OCS-APIRequest / Bearer / safe-method
// bypass rules and rejects everything else with 412 Precondition Failed,
// matching upstream Nextcloud's behaviour for missing requesttoken.
type CSRFConfig struct {
	Validate func(*http.Request) bool
}

// CSRF returns middleware that enforces CSRF protection for unsafe
// methods. Requests bearing OCS-APIRequest: true or Authorization: Bearer
// are passed through unchanged; all other unsafe requests must satisfy
// cfg.Validate (when set).
func CSRF(cfg CSRFConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsSafeMethod(r.Method) || IsOCSRequest(r) || IsBearerRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			if cfg.Validate != nil && cfg.Validate(r) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte("CSRF check failed\n"))
		})
	}
}
