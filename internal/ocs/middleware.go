package ocs

import (
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

const wwwAuthenticateValue = `Basic realm="Authorisation Required"`

func BasicAuth(version Version, verifier auth.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := auth.ParseBasicHeader(r.Header.Get("Authorization"))
			if !ok {
				writeUnauthorized(w, r, version)
				return
			}
			principal, err := verifier.Verify(r.Context(), user, pass)
			if err != nil {
				writeUnauthorized(w, r, version)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), principal)))
		})
	}
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
