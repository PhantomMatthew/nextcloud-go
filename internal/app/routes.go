package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/capabilities"
	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/login"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/status"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/web"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

type appPasswordIssuer struct {
	store  auth.Store
	secret string
}

func (a *appPasswordIssuer) Issue(r *http.Request, principal *auth.Principal) (string, error) {
	name := r.Header.Get("User-Agent")
	if name == "" {
		name = "unknown client"
	}
	raw, _, err := auth.IssueAppPassword(
		r.Context(),
		a.store,
		a.secret,
		principal.UID,
		principal.UID,
		name,
		auth.TokenTypePermanent,
	)
	return raw, err
}

func (a *appPasswordIssuer) Revoke(r *http.Request, _ *auth.Principal, raw string) error {
	return auth.RevokeAppPassword(r.Context(), a.store, a.secret, raw)
}

func (a *App) mountRoutes() error {
	maintenance := httpx.MaintenanceFunc(func() bool {
		return a.Cfg.Maintenance.Enabled
	})
	csrfCfg := httpx.CSRFConfig{
		PathBypass: []string{
			"/index.php/login/v2",
			"/index.php/login/v2/poll",
			"/index.php/login/v2/grant",
			"/remote.php/dav/",
			"/remote.php/webdav/",
		},
	}
	baseChain := []httpx.Middleware{
		httpx.Recover(a.Logger),
		httpx.RequestID(),
		httpx.Logging(a.Logger),
		httpx.SecurityHeaders(httpx.DefaultSecurityHeaders()),
		httpx.Maintenance(maintenance),
		httpx.CSRF(csrfCfg),
	}
	router := httpx.NewRouter(baseChain...)

	statusHandler := status.Provider{
		Installed:      true,
		Maintenance:    a.Cfg.Maintenance.Enabled,
		NeedsDBUpgrade: a.Cfg.Maintenance.NeedsDBUpgrade,
	}.Handler()
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		router.Handle(m, "/status.php", statusHandler)
	}

	capManager := capabilities.NewManager()
	capManager.Register(capabilities.DefaultCoreProvider())
	capHandler := capabilities.Handler{Manager: capManager}
	for _, m := range []string{"GET", "HEAD"} {
		router.Handle(m, "/ocs/v1.php/cloud/capabilities", capHandler.ServeOCS(ocs.V1))
		router.Handle(m, "/ocs/v2.php/cloud/capabilities", capHandler.ServeOCS(ocs.V2))
	}

	userVerifier := users.NewPasswordVerifier(a.Users, a.hasher)
	appPasswordVerifier := auth.NewAppPasswordVerifier(a.authStore, a.secret)
	verifier := auth.NewChainVerifier(appPasswordVerifier, userVerifier)
	userAccounts := authUsers{store: a.Users}
	authCfg := auth.MiddlewareConfig{
		Verifier: verifier,
		Bearer:   &auth.BearerVerifier{Store: a.authStore, Users: userAccounts, Secret: a.secret, Cache: a.Cache},
		Sessions: &auth.SessionVerifier{Sessions: a.sessions, Users: userAccounts},
		Throttle: auth.NewCacheThrottler(a.Cache, 8, 30*time.Second),
		Cookie:   session.CookieName,
	}
	issuer := &appPasswordIssuer{store: a.authStore, secret: a.secret}

	for _, m := range []string{"GET", "HEAD"} {
		router.Handle(m, "/ocs/v1.php/cloud/user", ocs.CloudUserHandler(ocs.V1), httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
		router.Handle(m, "/ocs/v2.php/cloud/user", ocs.CloudUserHandler(ocs.V2), httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))
	}
	for _, m := range []string{"GET", "HEAD"} {
		router.Handle(m, "/ocs/v1.php/core/getapppassword", ocs.GetAppPasswordHandler(ocs.V1, issuer), httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
		router.Handle(m, "/ocs/v2.php/core/getapppassword", ocs.GetAppPasswordHandler(ocs.V2, issuer), httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))
	}
	router.Handle("DELETE", "/ocs/v1.php/core/apppassword", ocs.DeleteAppPasswordHandler(ocs.V1, issuer), httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
	router.Handle("DELETE", "/ocs/v2.php/core/apppassword", ocs.DeleteAppPasswordHandler(ocs.V2, issuer), httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))

	loginSvc := login.NewService(a.loginStore)
	lv2 := web.NewLoginV2(loginSvc, verifier, issuer)
	lv2.Sessions = a.sessions
	lv2.Users = a.Users
	router.Handle(http.MethodPost, "/index.php/login/v2", http.HandlerFunc(lv2.HandleInit))
	router.Handle(http.MethodPost, "/index.php/login/v2/poll", http.HandlerFunc(lv2.HandlePoll))
	router.HandlePrefix(http.MethodGet, "/index.php/login/v2/flow/", http.HandlerFunc(lv2.HandleFlowToken))
	router.Handle(http.MethodGet, "/index.php/login/v2/flow", http.HandlerFunc(lv2.HandlePicker))
	router.Handle(http.MethodPost, "/index.php/login/v2/grant", http.HandlerFunc(lv2.HandleGrant))

	davHandler, err := webdav.NewHandler("/remote.php/dav/files/", a.davFS, a.instanceID)
	if err != nil {
		return fmt.Errorf("app: webdav: %w", err)
	}
	a.configureDAV(davHandler, false)
	router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/files/", webdav.Auth(authCfg)(davHandler))
	webdavRoot, err := webdav.NewHandler("/remote.php/webdav/", a.davFS, a.instanceID)
	if err != nil {
		return fmt.Errorf("app: webdav-root: %w", err)
	}
	a.configureDAV(webdavRoot, true)
	router.HandlePrefix(httpx.MethodAny, "/remote.php/webdav/", webdav.Auth(authCfg)(webdavRoot))

	a.Router = router
	return nil
}

func (a *App) configureDAV(h *webdav.Handler, ownerFromPrincipal bool) {
	if ownerFromPrincipal {
		h.OwnerUID = func(r *http.Request) string {
			p, ok := auth.UserFromContext(r.Context())
			if !ok {
				return ""
			}
			return p.UID
		}
	}
	h.OwnerName = func(uid string) string {
		u, err := a.Users.GetByUID(context.Background(), uid)
		if err != nil {
			return uid
		}
		if u.DisplayName != "" {
			return u.DisplayName
		}
		return uid
	}
	h.Quota = func(ctx context.Context, uid string) (used, available int64, unlimited bool) {
		if a.fileMeta == nil || a.Users == nil {
			return 0, -3, true
		}
		u, err := a.Users.GetByUID(ctx, uid)
		if err != nil {
			return 0, -3, true
		}
		used, err = a.fileMeta.Usage(ctx, u.ID)
		if err != nil {
			return 0, -3, true
		}
		if u.QuotaBytes == nil {
			return used, -3, true
		}
		return used, *u.QuotaBytes - used, false
	}
}
