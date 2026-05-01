package ocs

import (
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

type AppPasswordIssuer interface {
	Issue(r *http.Request, principal *auth.Principal) (string, error)
	Revoke(r *http.Request, principal *auth.Principal, raw string) error
}

func GetAppPasswordHandler(version Version, issuer AppPasswordIssuer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeUnauthorized(w, r, version)
			return
		}
		if principal.AuthMethod == auth.AuthMethodAppPassword {
			writeForbidden(w, r, version, "App password can't generate app password")
			return
		}
		token, err := issuer.Issue(r, principal)
		if err != nil {
			writeServerError(w, r, version)
			return
		}
		payload := Obj(K("apppassword", token))
		writeOK(w, r, version, payload)
	})
}

func DeleteAppPasswordHandler(version Version, issuer AppPasswordIssuer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeUnauthorized(w, r, version)
			return
		}
		if principal.AuthMethod != auth.AuthMethodAppPassword {
			writeForbidden(w, r, version, "no app password in use")
			return
		}
		_, raw, _ := auth.ParseBasicHeader(r.Header.Get("Authorization"))
		if err := issuer.Revoke(r, principal, raw); err != nil {
			writeServerError(w, r, version)
			return
		}
		writeOK(w, r, version, Obj())
	})
}

func writeOK(w http.ResponseWriter, r *http.Request, version Version, payload OrderedMap) {
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
}

func writeForbidden(w http.ResponseWriter, r *http.Request, version Version, message string) {
	format := NegotiateFormat(r.URL.Query().Get("format"), r.Header.Get("Accept"))
	meta := Meta{Status: "failure", StatusCode: http.StatusForbidden, Message: message}
	body, contentType, err := Render(version, format, meta, nil)
	if err != nil {
		http.Error(w, "render", http.StatusInternalServerError)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
}

func writeServerError(w http.ResponseWriter, r *http.Request, version Version) {
	format := NegotiateFormat(r.URL.Query().Get("format"), r.Header.Get("Accept"))
	meta := Meta{Status: "failure", StatusCode: RespondServerError, Message: "Internal Server Error"}
	body, contentType, err := Render(version, format, meta, nil)
	if err != nil {
		http.Error(w, "render", http.StatusInternalServerError)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(body)
}
