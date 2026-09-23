package app

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
)

func newMetricsApp(t *testing.T, enabled bool, token string) *App {
	t.Helper()
	return newMetricsAppListen(t, enabled, token, "")
}

func newMetricsAppListen(t *testing.T, enabled bool, token, listen string) *App {
	t.Helper()
	cfg := DevConfig()
	cfg.Database.DSN = "file:" + t.Name() + "?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Observability.MetricsEnabled = enabled
	cfg.Observability.MetricsToken = token
	cfg.Observability.MetricsListen = listen
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

func getMetrics(t *testing.T, a *App, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/metrics", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func TestMetricsEndpointDisabled(t *testing.T) {
	a := newMetricsApp(t, false, "")
	if rec := getMetrics(t, a, ""); rec.Code != 404 {
		t.Fatalf("disabled: status = %d, want 404", rec.Code)
	}
}

func TestMetricsEndpointNoToken(t *testing.T) {
	a := newMetricsApp(t, true, "")
	rec := getMetrics(t, a, "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, fam := range []string{
		"ncgo_plugin_host_calls_total",
		"ncgo_plugin_host_call_duration_seconds",
		"ncgo_plugin_capability_denials_total",
	} {
		if !strings.Contains(body, "# TYPE "+fam) {
			t.Errorf("body missing family %s:\n%s", fam, body)
		}
	}
}

func TestMetricsEndpointBearerToken(t *testing.T) {
	a := newMetricsApp(t, true, "scrape-token")
	if rec := getMetrics(t, a, ""); rec.Code != 401 {
		t.Errorf("no auth: status = %d, want 401", rec.Code)
	}
	if rec := getMetrics(t, a, "Bearer wrong"); rec.Code != 401 {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
	if rec := getMetrics(t, a, "Bearer scrape-token"); rec.Code != 200 {
		t.Errorf("correct token: status = %d, want 200", rec.Code)
	}
}

// TestMetricsDedicatedListener pins ADR-0076: with metrics_listen set,
// /metrics moves to the dedicated listener — the main router must 404 it
// (move, not copy), and the dedicated mux serves only GET /metrics behind
// the same bearer token.
func TestMetricsDedicatedListener(t *testing.T) {
	a := newMetricsAppListen(t, true, "scrape-token", "127.0.0.1:9090")

	// The main router no longer serves /metrics: the separate listener
	// exists for network-level restriction; serving both would defeat it.
	if rec := getMetrics(t, a, "Bearer scrape-token"); rec.Code != 404 {
		t.Errorf("main router GET /metrics: status = %d, want 404", rec.Code)
	}
	if a.metricsServer() == nil {
		t.Fatal("metricsServer() = nil with metrics_listen set")
	}

	serve := func(method, path, authHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		rec := httptest.NewRecorder()
		a.metricsHandler().ServeHTTP(rec, req)
		return rec
	}
	if rec := serve("GET", "/metrics", "Bearer scrape-token"); rec.Code != 200 {
		t.Errorf("dedicated GET /metrics with token: status = %d, want 200", rec.Code)
	}
	if rec := serve("GET", "/metrics", ""); rec.Code != 401 {
		t.Errorf("dedicated GET /metrics without token: status = %d, want 401", rec.Code)
	}
	if rec := serve("GET", "/status.php", "Bearer scrape-token"); rec.Code != 404 {
		t.Errorf("dedicated GET /status.php: status = %d, want 404", rec.Code)
	}
	if rec := serve("POST", "/metrics", "Bearer scrape-token"); rec.Code != 405 {
		t.Errorf("dedicated POST /metrics: status = %d, want 405", rec.Code)
	}
}

// TestMetricsNoDedicatedListener guards the pre-ADR-0076 behavior: with
// metrics_listen empty, /metrics stays on the main router and no dedicated
// server is built.
func TestMetricsNoDedicatedListener(t *testing.T) {
	a := newMetricsApp(t, true, "")
	if a.metricsServer() != nil {
		t.Error("metricsServer() != nil with metrics_listen empty")
	}
	if rec := getMetrics(t, a, ""); rec.Code != 200 {
		t.Errorf("main router GET /metrics: status = %d, want 200", rec.Code)
	}
}
