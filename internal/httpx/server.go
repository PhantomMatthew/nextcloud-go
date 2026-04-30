package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = 60 * time.Second
	defaultWriteTimeout      = 120 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultShutdownTimeout   = 30 * time.Second
)

type ServerConfig struct {
	Addr              string
	Handler           http.Handler
	Logger            *slog.Logger
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	BaseContext       func(net.Listener) context.Context
}

type Server struct {
	srv             *http.Server
	logger          *slog.Logger
	shutdownTimeout time.Duration
	mu              sync.Mutex
	closed          bool
}

func NewServer(cfg ServerConfig) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           cfg.Handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		BaseContext:       cfg.BaseContext,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	return &Server{
		srv:             srv,
		logger:          logger,
		shutdownTimeout: cfg.ShutdownTimeout,
	}
}

// Run starts the server and blocks until ctx is cancelled or
// ListenAndServe returns. On context cancellation it triggers a graceful
// shutdown bounded by ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("http server starting", "addr", s.srv.Addr)
		err := s.srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		s.logger.Info("http server shutting down", "timeout", s.shutdownTimeout)
		if err := s.shutdown(shutdownCtx); err != nil {
			s.logger.Error("http server shutdown failed", "error", err)
			return err
		}
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.srv.Shutdown(ctx)
}

// Addr returns the listening address configured for the underlying
// server, useful for tests that bind to ":0" and need to discover the
// chosen port via the listener.
func (s *Server) Addr() string {
	return s.srv.Addr
}
