package sharing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// TokenVerifier authenticates public DAV: username is the share token.
// Accepts public-link and OCM remote-share tokens (ADR-0023: the remote
// server authenticates with Basic(token, "") to pull share content).
type TokenVerifier struct {
	Service *Service
}

func (v *TokenVerifier) Verify(ctx context.Context, user, password string) (*auth.Principal, error) {
	if v == nil || v.Service == nil || user == "" {
		return nil, auth.ErrInvalidCredentials
	}
	sh, _, err := v.Service.ResolvePublicDAV(ctx, user, password)
	if err != nil {
		return nil, auth.ErrInvalidCredentials
	}
	return &auth.Principal{UID: sh.Token, Enabled: true, AuthMethod: auth.AuthMethodBasic}, nil
}

// PublicLinkHandler serves GET/HEAD /s/{token}.
func (s *Service) PublicLinkHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		token := strings.Trim(strings.TrimPrefix(r.URL.Path, "/s/"), "/")
		if token == "" || strings.Contains(token, "/") {
			http.NotFound(w, r)
			return
		}
		password := ""
		if user, pass, ok := auth.ParseBasicHeader(r.Header.Get("Authorization")); ok {
			if user != token {
				w.Header().Set("WWW-Authenticate", `Basic realm="Authorisation Required"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			password = pass
		}
		sh, owner, err := s.ResolvePublic(r.Context(), token, password)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				w.Header().Set("WWW-Authenticate", `Basic realm="Authorisation Required"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			http.NotFound(w, r)
			return
		}
		if sh.ItemType == "folder" {
			w.Header().Set("WWW-Authenticate", `Basic realm="Authorisation Required"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		// ADR-0104 phase 3a: a scheme-1 owner's share row carries the
		// ciphertext path; open the sealed metadata in the anonymous ctx.
		// Master-wrapped owners resolve through their own rows; an enrolled
		// owner without an unlocked session is ErrKeyLocked → 403 — the same
		// boundary as content (ADR-0101).
		plainPath := sh.Path
		if s.NameCodec != nil && sh.AbsPathEnc != "" {
			plain, _, err := s.NameCodec.OpenShareMeta(r.Context(), sh.OwnerUserID, sh)
			if err != nil {
				if errors.Is(err, encrypt.ErrKeyLocked) {
					http.Error(w, "encrypted: key locked", http.StatusForbidden)
					return
				}
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			plainPath = plain
		}
		rc, ent, err := s.Files.Read(r.Context(), owner.UID, plainPath)
		if err != nil {
			if errors.Is(err, webdav.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		if ent.ContentType != "" {
			w.Header().Set("Content-Type", ent.ContentType)
		}
		if ent.ETag != "" {
			w.Header().Set("ETag", `"`+ent.ETag+`"`)
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
	})
}

// RewritePublicDAVPath adds a trailing slash so WebDAV prefix matching works.
func RewritePublicDAVPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/public.php/webdav" {
			cp := *r
			u := *r.URL
			u.Path = "/public.php/webdav/"
			cp.URL = &u
			next.ServeHTTP(w, &cp)
			return
		}
		next.ServeHTTP(w, r)
	})
}
