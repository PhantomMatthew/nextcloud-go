package webdav

import (
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

const wwwAuthenticateValue = `Basic realm="Authorisation Required"`

func BasicAuth(verifier auth.Verifier) func(http.Handler) http.Handler {
	return Auth(auth.MiddlewareConfig{Verifier: verifier})
}

func Auth(cfg auth.MiddlewareConfig) func(http.Handler) http.Handler {
	cfg.Action = "webdav"
	cfg.OnAuthFail = func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeWebDAVUnauthorized(w)
	}
	return auth.Middleware(cfg)
}

func writeWebDAVUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", wwwAuthenticateValue)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusUnauthorized)
}
