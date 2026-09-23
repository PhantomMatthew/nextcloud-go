package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// startSpan wraps the whole router in one recording span, standing in for an
// outer tracing layer (the base-chain middleware starts its span after
// dispatch and instead reads the matched route from the context).
func startSpan(tp trace.TracerProvider) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, span := tp.Tracer("test").Start(r.Context(), r.Method)
			defer span.End()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func TestRouterRenamesActiveSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() {
		// Background, not t.Context(): the test context is already canceled
		// when cleanups run, and Shutdown needs a live one.
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("provider shutdown: %v", err)
		}
	})

	r := NewRouter()
	var routeFromCtx string
	r.Handle(http.MethodGet, "/status.php", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		routeFromCtx = RouteNameFromContext(req.Context())
		w.WriteHeader(http.StatusOK)
	}))
	r.HandlePrefix(MethodAny, "/remote.php/dav", okHandler("dav"))

	outer := startSpan(tp)(r)
	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status.php", nil))
	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/remote.php/dav/files/u/f.txt", nil))

	if routeFromCtx != "/status.php" {
		t.Errorf("route name in ctx = %q, want /status.php", routeFromCtx)
	}
	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(spans))
	}
	names := map[string]bool{spans[0].Name(): true, spans[1].Name(): true}
	if !names["GET /status.php"] || !names["GET /remote.php/dav"] {
		t.Errorf("span names = %q, %q", spans[0].Name(), spans[1].Name())
	}
}

func TestRouterNoSpanNoRouteNamePassthrough(t *testing.T) {
	// Without any span the rename hook is a no-op and the context still
	// carries the route for downstream consumers.
	r := NewRouter()
	r.Handle(http.MethodGet, "/x", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if n := RouteNameFromContext(req.Context()); n != "/x" {
			t.Errorf("route name = %q", n)
		}
		w.WriteHeader(http.StatusOK)
	}))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
}
