package app

import (
	"context"
	"log/slog"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
)

func newTracingApp(t *testing.T, endpoint string) *App {
	t.Helper()
	cfg := DevConfig()
	cfg.Database.DSN = "file:" + t.Name() + "?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Observability.OTelEndpoint = endpoint
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

func TestTracingDisabledByDefault(t *testing.T) {
	// Empty endpoint: no provider is constructed at all — the global default
	// stays the no-op provider (ADR-0072).
	a := newTracingApp(t, "")
	if a.tracing != nil {
		t.Error("provider constructed despite empty endpoint")
	}
}

func TestTracingConfiguredBuildsProvider(t *testing.T) {
	// The OTLP/HTTP exporter dials lazily, so construction and Close succeed
	// with no collector listening.
	a := newTracingApp(t, "127.0.0.1:4318")
	if a.tracing == nil {
		t.Fatal("provider not constructed")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("Close = %v", err)
	}
	if a.tracing != nil {
		t.Error("provider still set after Close")
	}
}

func TestTracingBadEndpointFailsStartup(t *testing.T) {
	cfg := DevConfig()
	cfg.Database.DSN = "file:" + t.Name() + "?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Observability.OTelEndpoint = "grpc://otel:4317"
	if _, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("startup must reject a non-HTTP endpoint scheme")
	}
}
