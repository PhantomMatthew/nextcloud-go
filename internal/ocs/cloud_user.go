package ocs

import (
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

func CloudUserHandler(version Version) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeUnauthorized(w, r, version)
			return
		}
		payload := Obj(
			K("id", principal.UID),
			K("display-name", principal.DisplayName),
			K("enabled", principal.Enabled),
		)
		format := NegotiateFormat(r.URL.Query().Get("format"), r.Header.Get("Accept"))
		body, contentType, err := Render(version, format, Meta{}, payload)
		if err != nil {
			http.Error(w, "render", http.StatusInternalServerError)
			return
		}
		okCode := StatusOKv1
		if version == V2 {
			okCode = StatusOKv2
		}
		hdr := w.Header()
		hdr.Set("Content-Type", contentType)
		w.WriteHeader(Map(version, okCode))
		_, _ = w.Write(body)
	})
}
