package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/capabilities"
	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/login"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/status"
	"github.com/PhantomMatthew/nextcloud-go/internal/web"
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

func loadSecret(logger *slog.Logger) string {
	if s := os.Getenv("NCGO_SECRET"); s != "" {
		return s
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		logger.Error("failed to generate ephemeral secret", "error", err)
		os.Exit(1)
	}
	logger.Warn("NCGO_SECRET not set; generated ephemeral secret (tokens will not survive restart)")
	return hex.EncodeToString(buf)
}

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("ncgo %s (commit %s, built %s)\n",
			observability.Version, observability.Commit, observability.BuildDate)
		return
	}

	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	maintenance := httpx.MaintenanceFunc(func() bool { return false })

	csrfCfg := httpx.CSRFConfig{
		PathBypass: []string{
			"/index.php/login/v2",
			"/index.php/login/v2/poll",
			"/index.php/login/v2/grant",
		},
	}

	baseChain := []httpx.Middleware{
		httpx.Recover(logger),
		httpx.RequestID(),
		httpx.Logging(logger),
		httpx.SecurityHeaders(httpx.DefaultSecurityHeaders()),
		httpx.Maintenance(maintenance),
		httpx.CSRF(csrfCfg),
	}

	router := httpx.NewRouter(baseChain...)

	statusHandler := status.Provider{}.Handler()
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

	secret := loadSecret(logger)
	tokenStore := auth.NewMemoryStore()
	staticVerifier := auth.NewStaticVerifier("admin", "admin", "admin")
	appPasswordVerifier := auth.NewAppPasswordVerifier(tokenStore, secret)
	verifier := auth.NewChainVerifier(appPasswordVerifier, staticVerifier)
	issuer := &appPasswordIssuer{store: tokenStore, secret: secret}

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

	loginStore := login.NewMemoryStore()
	loginStore.StartGC(0)
	defer loginStore.Close()
	loginSvc := login.NewService(loginStore)
	lv2 := web.NewLoginV2(loginSvc, verifier, issuer)

	router.Handle(http.MethodPost, "/index.php/login/v2", http.HandlerFunc(lv2.HandleInit))
	router.Handle(http.MethodPost, "/index.php/login/v2/poll", http.HandlerFunc(lv2.HandlePoll))
	router.HandlePrefix(http.MethodGet, "/index.php/login/v2/flow/", http.HandlerFunc(lv2.HandleFlowToken))
	router.Handle(http.MethodGet, "/index.php/login/v2/flow", http.HandlerFunc(lv2.HandlePicker))
	router.Handle(http.MethodPost, "/index.php/login/v2/grant", http.HandlerFunc(lv2.HandleGrant))

	srv := httpx.NewServer(httpx.ServerConfig{
		Addr:    *addr,
		Handler: router,
		Logger:  logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}
