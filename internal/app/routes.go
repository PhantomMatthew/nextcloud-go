package app

import (
	"fmt"
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/capabilities"
	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/login"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
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
	issuer := &appPasswordIssuer{store: a.authStore, secret: a.secret}

	for _, m := range []string{"GET", "HEAD"} {
		router.Handle(m, "/ocs/v1.php/cloud/user", ocs.CloudUserHandler(ocs.V1), httpx.Middleware(ocs.BasicAuth(ocs.V1, verifier)))
		router.Handle(m, "/ocs/v2.php/cloud/user", ocs.CloudUserHandler(ocs.V2), httpx.Middleware(ocs.BasicAuth(ocs.V2, verifier)))
	}
	for _, m := range []string{"GET", "HEAD"} {
		router.Handle(m, "/ocs/v1.php/core/getapppassword", ocs.GetAppPasswordHandler(ocs.V1, issuer), httpx.Middleware(ocs.BasicAuth(ocs.V1, verifier)))
		router.Handle(m, "/ocs/v2.php/core/getapppassword", ocs.GetAppPasswordHandler(ocs.V2, issuer), httpx.Middleware(ocs.BasicAuth(ocs.V2, verifier)))
	}
	router.Handle("DELETE", "/ocs/v1.php/core/apppassword", ocs.DeleteAppPasswordHandler(ocs.V1, issuer), httpx.Middleware(ocs.BasicAuth(ocs.V1, verifier)))
	router.Handle("DELETE", "/ocs/v2.php/core/apppassword", ocs.DeleteAppPasswordHandler(ocs.V2, issuer), httpx.Middleware(ocs.BasicAuth(ocs.V2, verifier)))

	loginSvc := login.NewService(a.loginStore)
	lv2 := web.NewLoginV2(loginSvc, verifier, issuer)
	router.Handle(http.MethodPost, "/index.php/login/v2", http.HandlerFunc(lv2.HandleInit))
	router.Handle(http.MethodPost, "/index.php/login/v2/poll", http.HandlerFunc(lv2.HandlePoll))
	router.HandlePrefix(http.MethodGet, "/index.php/login/v2/flow/", http.HandlerFunc(lv2.HandleFlowToken))
	router.Handle(http.MethodGet, "/index.php/login/v2/flow", http.HandlerFunc(lv2.HandlePicker))
	router.Handle(http.MethodPost, "/index.php/login/v2/grant", http.HandlerFunc(lv2.HandleGrant))

	davHandler, err := webdav.NewHandler("/remote.php/dav/files/", webdav.NewInMemoryFS(), a.instanceID)
	if err != nil {
		return fmt.Errorf("app: webdav: %w", err)
	}
	router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/files/", webdav.BasicAuth(verifier)(davHandler))

	a.Router = router
	return nil
}
