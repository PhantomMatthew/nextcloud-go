package ocs

import (
	"context"
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

type AppPasswordIssuer interface {
	// Issue mints a permanent app password for the principal, returning the
	// raw token and the token id (the id names the app_passwords row — the
	// ADR-0102 key wrap keys off it).
	Issue(r *http.Request, principal *auth.Principal) (raw, tokenID string, err error)
	Revoke(r *http.Request, principal *auth.Principal, raw string) error
}

// AppTokenKeyWrapper seals an unlocked private key under a newly issued app
// password's raw token (ADR-0102 wrap-at-issuance). *encrypt.SQLResolver
// satisfies it structurally; ocs must not import encrypt, so the seam is
// declared here.
type AppTokenKeyWrapper interface {
	WrapKeyForToken(ctx context.Context, appPasswordID, tokenRaw string, priv []byte) error
}

func GetAppPasswordHandler(version Version, issuer AppPasswordIssuer, keys AppTokenKeyWrapper) http.Handler {
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
		token, tokenID, err := issuer.Issue(r, principal)
		if err != nil {
			writeServerError(w, r, version)
			return
		}
		// Wrap-at-issuance (ADR-0102), mirroring the login-v2 grant: when the
		// authenticating principal carries an unlocked key (a session-
		// authenticated request of an enrolled user — the 4-a middleware
		// attach), seal it under the new token so the token's requests can
		// unlock files. A wrap failure fails LOUDLY: a silently unwrapped
		// token would 403 every file. Without a key (basic/bearer issuance)
		// the token authenticates but cannot unlock files until it is
		// re-issued from an unlocked session — the pre-enrollment behavior.
		if principal.UnlockedKey != nil && keys != nil {
			if err := keys.WrapKeyForToken(r.Context(), tokenID, token, principal.UnlockedKey); err != nil {
				writeServerError(w, r, version)
				return
			}
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
