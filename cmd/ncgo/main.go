package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/status"
)

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("ncgo %s (commit %s, built %s)\n",
			observability.Version, observability.Commit, observability.BuildDate)
		return
	}

	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	baseChain := []httpx.Middleware{
		httpx.Recover(logger),
		httpx.RequestID(),
		httpx.Logging(logger),
		httpx.SecurityHeaders(httpx.DefaultSecurityHeaders()),
	}

	router := httpx.NewRouter(baseChain...)
	statusHandler := status.Provider{}.Handler()
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		router.Handle(m, "/status.php", statusHandler)
	}

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
