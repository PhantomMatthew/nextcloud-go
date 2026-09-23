package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	actpkg "github.com/PhantomMatthew/nextcloud-go/internal/activity"
	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/capabilities"
	"github.com/PhantomMatthew/nextcloud-go/internal/console"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/login"
	notifpkg "github.com/PhantomMatthew/nextcloud-go/internal/notifications"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocm"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins"
	"github.com/PhantomMatthew/nextcloud-go/internal/search"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
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
			"/index.php/login",
			"/index.php/logout",
			"/remote.php/dav/",
			"/remote.php/webdav/",
			"/public.php/webdav",
			"/public.php/webdav/",
			"/ocm/",
			"/.well-known/caldav",
			"/.well-known/carddav",
		},
		// Requests carrying the session cookie defer to the auth middleware,
		// whose session branch performs the real requesttoken check (403).
		// /index.php/login and /index.php/logout stay path-bypassed because
		// their handlers validate the anonymous login token themselves.
		SessionCookie: session.CookieName,
	}
	baseChain := []httpx.Middleware{
		httpx.Recover(a.Logger),
	}
	// Tracing sits directly behind Recover: downstream panics are recorded as
	// error spans before Recover converts them to 500s (ADR-0072).
	if a.tracing != nil {
		baseChain = append(baseChain, observability.Tracing(a.tracing))
	}
	baseChain = append(baseChain,
		httpx.RequestID(),
		httpx.Logging(a.Logger),
		httpx.SecurityHeaders(httpx.DefaultSecurityHeaders()),
		httpx.Maintenance(maintenance),
		httpx.CSRF(csrfCfg),
	)
	router := httpx.NewRouter(baseChain...)

	statusProvider := status.Provider{
		Installed:      true,
		Maintenance:    a.Cfg.Maintenance.Enabled,
		NeedsDBUpgrade: a.Cfg.Maintenance.NeedsDBUpgrade,
	}
	statusHandler := statusProvider.Handler()
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		router.Handle(m, "/status.php", statusHandler)
	}

	capManager := capabilities.NewManager()
	capManager.Register(capabilities.DefaultCoreProvider())
	capManager.Register(capabilities.DefaultDAVProvider())
	capManager.Register(capabilities.DefaultFilesProvider())
	capManager.Register(capabilities.DefaultSharingProvider())
	capHandler := capabilities.Handler{Manager: capManager}
	for _, m := range []string{"GET", "HEAD"} {
		router.Handle(m, "/ocs/v1.php/cloud/capabilities", capHandler.ServeOCS(ocs.V1))
		router.Handle(m, "/ocs/v2.php/cloud/capabilities", capHandler.ServeOCS(ocs.V2))
	}

	userVerifier := users.NewPasswordVerifier(a.Users, a.hasher)
	appPasswordVerifier := auth.NewAppPasswordVerifier(a.authStore, a.secret)
	verifier := auth.NewChainVerifier(appPasswordVerifier, userVerifier)
	userAccounts := authUsers{store: a.Users}
	requestTokens := auth.NewRequestToken(a.secret)
	authCfg := auth.MiddlewareConfig{
		Verifier:     verifier,
		Bearer:       &auth.BearerVerifier{Store: a.authStore, Users: userAccounts, Secret: a.secret, Cache: a.Cache},
		Sessions:     &auth.SessionVerifier{Sessions: a.sessions, Users: userAccounts},
		Throttle:     auth.NewCacheThrottler(a.Cache, 8, 30*time.Second),
		Cookie:       session.CookieName,
		RequestToken: requestTokens,
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

	searchFiles := search.NewFilesProvider(a.fileMeta, a.Users)
	searchV1 := search.Handler{Providers: []search.Provider{searchFiles}, Version: ocs.V1}
	searchV2 := search.Handler{Providers: []search.Provider{searchFiles}, Version: ocs.V2}
	router.HandlePrefix(httpx.MethodAny, "/ocs/v1.php/search/providers", searchV1, httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
	router.HandlePrefix(httpx.MethodAny, "/ocs/v2.php/search/providers", searchV2, httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))

	if dav, ok := a.davFS.(*files.DAV); ok {
		lockV1 := files.LockHandler{DAV: dav, Version: ocs.V1}
		lockV2 := files.LockHandler{DAV: dav, Version: ocs.V2}
		router.HandlePrefix(httpx.MethodAny, "/ocs/v1.php/apps/files_lock", lockV1, httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
		router.HandlePrefix(httpx.MethodAny, "/ocs/v2.php/apps/files_lock", lockV2, httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))
	}

	if a.shares != nil {
		sharesV1 := sharing.Handler{Service: a.shares, Version: ocs.V1}
		sharesV2 := sharing.Handler{Service: a.shares, Version: ocs.V2}
		shareesV1 := sharing.ShareesHandler{Version: ocs.V1, Lookup: a.lookup, Users: a.Users, Shares: a.shares.Store}
		shareesV2 := sharing.ShareesHandler{Version: ocs.V2, Lookup: a.lookup, Users: a.Users, Shares: a.shares.Store}
		router.HandlePrefix(httpx.MethodAny, "/ocs/v1.php/apps/files_sharing/api/v1/shares", sharesV1, httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
		router.HandlePrefix(httpx.MethodAny, "/ocs/v2.php/apps/files_sharing/api/v1/shares", sharesV2, httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))
		router.HandlePrefix(http.MethodGet, "/ocs/v1.php/apps/files_sharing/api/v1/sharees", shareesV1, httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
		router.HandlePrefix(http.MethodGet, "/ocs/v2.php/apps/files_sharing/api/v1/sharees", shareesV2, httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))
		router.HandlePrefix(http.MethodGet, "/s/", a.shares.PublicLinkHandler())
		router.HandlePrefix(http.MethodHead, "/s/", a.shares.PublicLinkHandler())
	}

	router.Handle(http.MethodGet, "/.well-known/ocm", http.HandlerFunc(ocm.Discovery))
	router.Handle(http.MethodHead, "/.well-known/ocm", http.HandlerFunc(ocm.Discovery))
	router.Handle(http.MethodGet, "/ocm-provider", http.HandlerFunc(ocm.Discovery))
	router.Handle(http.MethodHead, "/ocm-provider", http.HandlerFunc(ocm.Discovery))
	router.Handle(http.MethodGet, "/ocm-provider/", http.HandlerFunc(ocm.Discovery))
	router.Handle(http.MethodHead, "/ocm-provider/", http.HandlerFunc(ocm.Discovery))
	if a.ocmStore != nil {
		inc := ocm.IncomingHandler{Store: a.ocmStore, Users: a.Users}
		router.HandlePrefix(httpx.MethodAny, "/ocm/", inc)
		remoteV1 := ocm.RemoteSharesHandler{Store: a.ocmStore, Users: a.Users, Version: ocs.V1}
		remoteV2 := ocm.RemoteSharesHandler{Store: a.ocmStore, Users: a.Users, Version: ocs.V2}
		router.HandlePrefix(httpx.MethodAny, "/ocs/v1.php/apps/files_sharing/api/v1/remote_shares", remoteV1, httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
		router.HandlePrefix(httpx.MethodAny, "/ocs/v2.php/apps/files_sharing/api/v1/remote_shares", remoteV2, httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))
	}

	if a.notifStore != nil {
		notifV1 := notifpkg.Handler{Store: a.notifStore, Users: a.Users, Version: ocs.V1}
		notifV2 := notifpkg.Handler{Store: a.notifStore, Users: a.Users, Version: ocs.V2}
		router.HandlePrefix(httpx.MethodAny, "/ocs/v1.php/apps/notifications/api/v2/notifications", notifV1, httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
		router.HandlePrefix(httpx.MethodAny, "/ocs/v2.php/apps/notifications/api/v2/notifications", notifV2, httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))
	}
	if a.activityStore != nil {
		actV1 := actpkg.Handler{Store: a.activityStore, Users: a.Users, Version: ocs.V1}
		actV2 := actpkg.Handler{Store: a.activityStore, Users: a.Users, Version: ocs.V2}
		router.HandlePrefix(httpx.MethodAny, "/ocs/v1.php/apps/activity/api/v2/activity", actV1, httpx.Middleware(ocs.Auth(ocs.V1, authCfg)))
		router.HandlePrefix(httpx.MethodAny, "/ocs/v2.php/apps/activity/api/v2/activity", actV2, httpx.Middleware(ocs.Auth(ocs.V2, authCfg)))
	}

	loginSvc := login.NewService(a.loginStore)
	lv2 := web.NewLoginV2(loginSvc, verifier, issuer)
	lv2.Sessions = a.sessions
	lv2.Users = a.Users
	router.Handle(http.MethodPost, "/index.php/login/v2", http.HandlerFunc(lv2.HandleInit))
	router.Handle(http.MethodPost, "/index.php/login/v2/poll", http.HandlerFunc(lv2.HandlePoll))
	router.HandlePrefix(http.MethodGet, "/index.php/login/v2/flow/", http.HandlerFunc(lv2.HandleFlowToken))
	router.Handle(http.MethodGet, "/index.php/login/v2/flow", http.HandlerFunc(lv2.HandlePicker))
	router.Handle(http.MethodPost, "/index.php/login/v2/grant", http.HandlerFunc(lv2.HandleGrant))

	browserLogin := &web.BrowserLogin{
		Verifier: verifier,
		Users:    a.Users,
		Sessions: a.sessions,
		Tokens:   requestTokens,
		Throttle: authCfg.Throttle,
	}
	router.Handle(http.MethodPost, "/index.php/login", http.HandlerFunc(browserLogin.HandleLogin))
	router.Handle(http.MethodGet, "/index.php/logout", http.HandlerFunc(browserLogin.HandleLogout))
	router.Handle(http.MethodPost, "/index.php/logout", http.HandlerFunc(browserLogin.HandleLogout))

	// The embedded admin console (ADR-0080) mounts unconditionally: it is the
	// zero-config alternative to pointing web.static_root at a Nextcloud
	// release. console.Auth is the usual credential stack with a
	// console-shaped failure writer (login-link page / JSON 401 instead of
	// the empty WebDAV challenge); RequireAdmin then gates to the admin
	// group. GET/HEAD-only: the router 405s other methods, and the longest-
	// prefix rules keep these ahead of the static catch-all below.
	consoleHandler := &console.Handler{
		Users:      a.Users,
		Jobs:       a.jobsStore,
		DB:         a.DB,
		Cfg:        a.Cfg,
		InstanceID: a.instanceID,
		Status:     statusProvider,
	}
	consoleMw := []httpx.Middleware{
		httpx.Middleware(console.Auth(authCfg)),
		httpx.Middleware(console.RequireAdmin(a.Users)),
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		router.Handle(m, "/console", consoleHandler, consoleMw...)
		router.HandlePrefix(m, "/console/", consoleHandler, consoleMw...)
	}

	if a.previewGen != nil {
		// The extensionless pair is what the NC web UI generates when
		// mod_rewrite works — and the SPA bootstrap advertises exactly that
		// (window._oc_config modRewriteWorking: true, ADR-0069), so without
		// these mounts the UI's own preview URLs 404 (ADR-0077).
		for _, p := range []string{"/index.php/core/preview", "/index.php/core/preview.png", "/core/preview", "/core/preview.png"} {
			router.Handle(http.MethodGet, p, a.previewGen, httpx.Middleware(webdav.Auth(authCfg)))
		}
	}

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

	if up, ok := a.uploadsFS.(*files.Uploads); ok {
		uploadsHandler, err := webdav.NewHandler("/remote.php/dav/uploads/", up, a.instanceID)
		if err != nil {
			return fmt.Errorf("app: uploads: %w", err)
		}
		uploadsHandler.FilesPrefix = "/remote.php/dav/files/"
		uploadsHandler.Assemble = up.Assemble
		a.configureDAV(uploadsHandler, false)
		router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/uploads/", webdav.Auth(authCfg)(uploadsHandler))
	}

	if a.trashFS != nil {
		trashHandler, err := webdav.NewHandler("/remote.php/dav/trashbin/", a.trashFS, a.instanceID)
		if err != nil {
			return fmt.Errorf("app: trashbin: %w", err)
		}
		trashHandler.FilesPrefix = "/remote.php/dav/files/"
		trashHandler.Restore = a.trashFS.Restore
		a.configureDAV(trashHandler, false)
		router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/trashbin/", webdav.Auth(authCfg)(trashHandler))
	}

	router.Handle(http.MethodGet, "/.well-known/caldav", http.HandlerFunc(wellKnownCalDAV))
	router.Handle(http.MethodHead, "/.well-known/caldav", http.HandlerFunc(wellKnownCalDAV))
	router.Handle(http.MethodGet, "/.well-known/carddav", http.HandlerFunc(wellKnownCalDAV))
	router.Handle(http.MethodHead, "/.well-known/carddav", http.HandlerFunc(wellKnownCalDAV))

	if a.calendarFS != nil {
		calHandler, err := webdav.NewHandler("/remote.php/dav/calendars/", a.calendarFS, a.instanceID)
		if err != nil {
			return fmt.Errorf("app: calendars: %w", err)
		}
		calHandler.DAVHeader = "1, 3, calendar-access, extended-mkcol"
		calHandler.Allow = "OPTIONS, GET, HEAD, PROPFIND, PUT, DELETE, MKCOL, MKCALENDAR, REPORT, PROPPATCH"
		a.configureDAV(calHandler, false)
		router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/calendars/", webdav.Auth(authCfg)(calHandler))
	}
	if a.contactsFS != nil {
		cardHandler, err := webdav.NewHandler("/remote.php/dav/addressbooks/users/", a.contactsFS, a.instanceID)
		if err != nil {
			return fmt.Errorf("app: addressbooks: %w", err)
		}
		cardHandler.DAVHeader = "1, 3, addressbook, extended-mkcol"
		cardHandler.Allow = "OPTIONS, GET, HEAD, PROPFIND, PUT, DELETE, MKCOL, REPORT, PROPPATCH"
		a.configureDAV(cardHandler, false)
		router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/addressbooks/users/", webdav.Auth(authCfg)(cardHandler))
	}
	if a.principalFS != nil {
		prinHandler, err := webdav.NewHandler("/remote.php/dav/principals/users/", a.principalFS, a.instanceID)
		if err != nil {
			return fmt.Errorf("app: principals: %w", err)
		}
		prinHandler.DAVHeader = "1, 3"
		prinHandler.Allow = "OPTIONS, PROPFIND"
		a.configureDAV(prinHandler, false)
		router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/principals/users/", webdav.Auth(authCfg)(prinHandler))
	}
	if a.davRootFS != nil {
		rootHandler, err := webdav.NewHandler("/remote.php/dav/", a.davRootFS, a.instanceID)
		if err != nil {
			return fmt.Errorf("app: dav-root: %w", err)
		}
		rootHandler.DAVHeader = "1, 3, calendar-access, addressbook, extended-mkcol"
		rootHandler.Allow = "OPTIONS, PROPFIND"
		a.configureDAV(rootHandler, true)
		router.HandlePrefix(httpx.MethodAny, "/remote.php/dav", webdav.Auth(authCfg)(rootHandler))
	}

	if a.versionsFS != nil {
		versionsHandler, err := webdav.NewHandler("/remote.php/dav/versions/", a.versionsFS, a.instanceID)
		if err != nil {
			return fmt.Errorf("app: versions: %w", err)
		}
		versionsHandler.RestoreVersion = a.versionsFS.RestoreVersion
		a.configureDAV(versionsHandler, false)
		router.HandlePrefix(httpx.MethodAny, "/remote.php/dav/versions/", webdav.Auth(authCfg)(versionsHandler))
	}

	if a.publicFS != nil && a.shares != nil {
		pubHandler, err := webdav.NewHandler("/public.php/webdav/", a.publicFS, a.instanceID)
		if err != nil {
			return fmt.Errorf("app: public-webdav: %w", err)
		}
		a.configureDAV(pubHandler, true)
		pubAuth := auth.MiddlewareConfig{
			Verifier: &sharing.TokenVerifier{Service: a.shares},
			Throttle: authCfg.Throttle,
		}
		pub := sharing.RewritePublicDAVPath(webdav.Auth(pubAuth)(pubHandler))
		router.HandlePrefix(httpx.MethodAny, "/public.php/webdav", pub)
	}

	if a.PluginHost != nil && a.pluginReg != nil {
		// The reconciler's first Sync is the boot-time mount (the former
		// StartEnabled + MountRoutes pair); its Run loop then hot-applies
		// later registry changes (ADR-0062). A registry outage here is
		// logged, not fatal: the next tick retries.
		rec := plugins.NewReconciler(a.PluginHost, a.pluginReg, router,
			webdav.Auth(authCfg),
			httpx.Middleware(ocs.Auth(ocs.V1, authCfg)),
			httpx.Middleware(ocs.Auth(ocs.V2, authCfg)),
			a.Logger)
		if err := rec.Sync(context.Background()); err != nil {
			a.Logger.ErrorContext(context.Background(), "plugins: initial reconcile failed", slog.String("error", err.Error()))
		}
		a.reconciler = rec
	}

	// With a dedicated metrics listener (observability.metrics_listen,
	// ADR-0076) /metrics moves off the main router entirely: the separate
	// listener exists for network-level restriction, and serving both would
	// defeat it.
	if a.metrics != nil && a.Cfg.Observability.MetricsListen == "" {
		router.Handle(http.MethodGet, "/metrics", a.metrics.Handler(a.Cfg.Observability.MetricsToken))
	}

	// Static frontend catch-all: the router's longest-prefix matching keeps
	// every exact and prefix route above ahead of this "/" mount, so only
	// paths nothing else claimed reach the SPA/static handler. Static GETs
	// are safe methods, so the CSRF chain passes them unchanged. The SPA
	// shell is injected with the per-session (or anonymous login) bootstrap
	// requesttoken and bootstrap state (ADR-0064, ADR-0069).
	if a.staticUI != nil {
		a.staticUI.Shell = &web.BrowserBootstrap{Sessions: a.sessions, Tokens: requestTokens, Users: a.Users}
		// The exact POST /index.php/login route would otherwise 405 GETs of
		// the login page; the shell (Vue login app) is that page.
		router.Handle(http.MethodGet, "/index.php/login", a.staticUI)
		router.Handle(http.MethodHead, "/index.php/login", a.staticUI)
		router.HandlePrefix(http.MethodGet, "/", a.staticUI)
		router.HandlePrefix(http.MethodHead, "/", a.staticUI)
	}

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

func wellKnownCalDAV(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Location", "/remote.php/dav/")
	w.WriteHeader(http.StatusMovedPermanently)
}
