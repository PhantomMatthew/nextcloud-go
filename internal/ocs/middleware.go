package ocs

import (
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

const wwwAuthenticateValue = `Basic realm="Authorisation Required"`

func BasicAuth(version Version, verifier auth.Verifier) func(http.Handler) http.Handler {
	return Auth(version, auth.MiddlewareConfig{Verifier: verifier})
}

func Auth(version Version, cfg auth.MiddlewareConfig) func(http.Handler) http.Handler {
	cfg.Action = "login"
	cfg.OnAuthFail = func(w http.ResponseWriter, r *http.Request, _ error) {
		writeUnauthorized(w, r, version)
	}
	return auth.Middleware(cfg)
}

func writeUnauthorized(w http.ResponseWriter, r *http.Request, version Version) {
	format := NegotiateFormat(r.URL.Query().Get("format"), r.Header.Get("Accept"))
	meta := Meta{
		Status:     "failure",
		StatusCode: RespondUnauthorised,
		Message:    "Current user is not logged in",
	}
	body, contentType, err := Render(version, format, meta, nil)
	if err != nil {
		http.Error(w, "render", http.StatusInternalServerError)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	hdr.Set("WWW-Authenticate", wwwAuthenticateValue)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(body)
}
