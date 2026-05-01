package webdav

import (
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

const wwwAuthenticateValue = `Basic realm="Authorisation Required"`

func BasicAuth(verifier auth.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := auth.ParseBasicHeader(r.Header.Get("Authorization"))
			if !ok {
				writeWebDAVUnauthorized(w)
				return
			}
			principal, err := verifier.Verify(r.Context(), user, pass)
			if err != nil {
				writeWebDAVUnauthorized(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), principal)))
		})
	}
}

func writeWebDAVUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", wwwAuthenticateValue)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusUnauthorized)
}
