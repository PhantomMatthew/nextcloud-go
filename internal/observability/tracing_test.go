package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
)

func recorderProvider(t *testing.T, opts ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(append([]sdktrace.TracerProviderOption{
		sdktrace.WithSpanProcessor(sr),
	}, opts...)...)
	t.Cleanup(func() {
		// Background, not t.Context(): the test context is already canceled
		// when cleanups run, and Shutdown needs a live one.
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("provider shutdown: %v", err)
		}
	})
	return tp, sr
}

// serveRouted registers routes on a router whose default chain is
// Tracing+RequestID (mirroring the app's base-chain order) and serves one
// request through it.
func serveRouted(t *testing.T, tp trace.TracerProvider, register func(*httpx.Router), req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	router := httpx.NewRouter(Tracing(tp), httpx.RequestID())
	register(router)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	return rw
}

func spanAttrs(t *testing.T, s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	t.Helper()
	m := make(map[string]attribute.Value, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func TestTracingExactRouteSpan(t *testing.T) {
	tp, sr := recorderProvider(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rw := serveRouted(t, tp, func(r *httpx.Router) { r.Handle(http.MethodGet, "/status.php", ok) },
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status.php", nil))
	if rw.Header().Get(httpx.HeaderRequestID) == "" {
		t.Fatal("no request id on response")
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "GET /status.php" {
		t.Errorf("span name = %q, want %q", s.Name(), "GET /status.php")
	}
	if s.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v", s.SpanKind())
	}
	if s.Status().Code != codes.Unset {
		t.Errorf("200 status code = %v, want Unset", s.Status().Code)
	}
	attrs := spanAttrs(t, s)
	if got := attrs["http.method"].AsString(); got != http.MethodGet {
		t.Errorf("http.method = %q", got)
	}
	if got := attrs["http.route"].AsString(); got != "/status.php" {
		t.Errorf("http.route = %q", got)
	}
	if got := attrs["http.target"].AsString(); got != "/status.php" {
		t.Errorf("http.target = %q", got)
	}
	if got := attrs["http.status_code"].AsInt64(); got != http.StatusOK {
		t.Errorf("http.status_code = %d", got)
	}
	if got := attrs["request_id"].AsString(); got == "" || got != rw.Header().Get(httpx.HeaderRequestID) {
		t.Errorf("request_id = %q, header %q", got, rw.Header().Get(httpx.HeaderRequestID))
	}
	// httptest.NewRequest sets RemoteAddr to 192.0.2.1:1234.
	if got := attrs["http.client_ip"].AsString(); got != "192.0.2.1" {
		t.Errorf("http.client_ip = %q", got)
	}
}

func TestTracingPrefixRouteSpan(t *testing.T) {
	tp, sr := recorderProvider(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	serveRouted(t, tp, func(r *httpx.Router) { r.HandlePrefix(httpx.MethodAny, "/remote.php/dav", ok) },
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/remote.php/dav/files/alice/Documents/report.pdf", nil))
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	s := spans[0]
	// The registered prefix names the span; the raw file path stays on
	// http.target only (DAV paths would explode span-name cardinality).
	if s.Name() != "GET /remote.php/dav" {
		t.Errorf("span name = %q, want %q", s.Name(), "GET /remote.php/dav")
	}
	attrs := spanAttrs(t, s)
	if got := attrs["http.route"].AsString(); got != "/remote.php/dav" {
		t.Errorf("http.route = %q", got)
	}
	if got := attrs["http.target"].AsString(); got != "/remote.php/dav/files/alice/Documents/report.pdf" {
		t.Errorf("http.target = %q", got)
	}
}

func TestTracingUnmatchedSpanIsMethodOnly(t *testing.T) {
	tp, sr := recorderProvider(t)
	// No router: the middleware alone has no route name, so the span keeps
	// its method-only initial name and no http.route attribute.
	h := Tracing(tp)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/anything", nil))
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != http.MethodDelete {
		t.Errorf("span name = %q, want method only", s.Name())
	}
	if _, ok := spanAttrs(t, s)["http.route"]; ok {
		t.Error("http.route must be absent without a matched route")
	}
}

func TestTracingServerErrorMarksSpan(t *testing.T) {
	tp, sr := recorderProvider(t)
	boom := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	serveRouted(t, tp, func(r *httpx.Router) { r.Handle(http.MethodGet, "/boom", boom) },
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", nil))
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Status().Code != codes.Error {
		t.Errorf("500 status code = %v, want Error", s.Status().Code)
	}
	if got := s.Status().Description; got != "HTTP 500" {
		t.Errorf("status description = %q", got)
	}
	if got := spanAttrs(t, s)["http.status_code"].AsInt64(); got != http.StatusInternalServerError {
		t.Errorf("http.status_code = %d", got)
	}
}

func TestTracingClientErrorStaysUnset(t *testing.T) {
	tp, sr := recorderProvider(t)
	missing := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	serveRouted(t, tp, func(r *httpx.Router) { r.Handle(http.MethodGet, "/missing", missing) },
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/missing", nil))
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if s := spans[0]; s.Status().Code != codes.Unset {
		t.Errorf("404 status code = %v, want Unset", s.Status().Code)
	}
}

func TestTracingTraceparentExtracted(t *testing.T) {
	tp, sr := recorderProvider(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status.php", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	serveRouted(t, tp, func(r *httpx.Router) { r.Handle(http.MethodGet, "/status.php", ok) }, req)
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if got, want := s.SpanContext().TraceID().String(), "4bf92f3577b34da6a3ce929d0e0e4736"; got != want {
		t.Errorf("trace id = %q, want upstream %q", got, want)
	}
	if got, want := s.Parent().SpanID().String(), "00f067aa0ba902b7"; got != want {
		t.Errorf("parent span id = %q, want upstream %q", got, want)
	}
}

func TestTracingInvalidTraceparentStartsNewRoot(t *testing.T) {
	tp, sr := recorderProvider(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status.php", nil)
	req.Header.Set("traceparent", "not-a-traceparent")
	serveRouted(t, tp, func(r *httpx.Router) { r.Handle(http.MethodGet, "/status.php", ok) }, req)
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if s := spans[0]; s.Parent().IsValid() {
		t.Errorf("invalid traceparent must start a new root, got parent %q", s.Parent().SpanID())
	}
}

func TestTracingPanicMarkedAndRepanics(t *testing.T) {
	tp, sr := recorderProvider(t)
	traced := Tracing(tp)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))
	recovered := httpx.Recover(nil)(traced)
	rw := httptest.NewRecorder()
	recovered.ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/panic", nil))
	if rw.Code != http.StatusInternalServerError {
		t.Errorf("panic response = %d", rw.Code)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if s := spans[0]; s.Status().Code != codes.Error {
		t.Errorf("panic span status = %v, want Error", s.Status().Code)
	}
}

func TestSamplerRatioZeroRecordsNothing(t *testing.T) {
	tp, sr := recorderProvider(t, sdktrace.WithSampler(Sampler(0)))
	ctx, span := tp.Tracer("test").Start(t.Context(), "dropped")
	span.End()
	_ = ctx
	if n := len(sr.Ended()); n != 0 {
		t.Errorf("ratio 0 recorded %d spans", n)
	}
}

func TestSamplerRatioOneRecordsEverything(t *testing.T) {
	tp, sr := recorderProvider(t, sdktrace.WithSampler(Sampler(1)))
	_, span := tp.Tracer("test").Start(t.Context(), "kept")
	span.End()
	if n := len(sr.Ended()); n != 1 {
		t.Errorf("ratio 1 recorded %d spans, want 1", n)
	}
}

func TestParseOTLPEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		endpoint string
		asURL    bool
		insecure bool
		wantErr  bool
	}{
		{"localhost:4318", false, true, false},
		{"otel-collector:4318", false, true, false},
		{"http://otel:4318", true, true, false},
		{"http://otel:4318/custom/path", true, true, false},
		{"https://otel.example.com:4318", true, false, false},
		{"  https://otel.example.com  ", true, false, false},
		{"grpc://otel:4317", false, false, true},
		{"", false, false, true},
		{"   ", false, false, true},
		{"://bad", false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			t.Parallel()
			asURL, insecure, err := parseOTLPEndpoint(tt.endpoint)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if asURL != tt.asURL || insecure != tt.insecure {
				t.Errorf("parseOTLPEndpoint(%q) = (%v, %v), want (%v, %v)", tt.endpoint, asURL, insecure, tt.asURL, tt.insecure)
			}
		})
	}
}

func TestNewTracerProviderRejectsBadEndpoint(t *testing.T) {
	if _, err := NewTracerProvider(t.Context(), TracingConfig{Endpoint: ""}); err == nil {
		t.Error("empty endpoint must fail")
	}
	if _, err := NewTracerProvider(t.Context(), TracingConfig{Endpoint: "grpc://otel:4317"}); err == nil {
		t.Error("grpc scheme must fail")
	}
}

func TestNewTracerProviderBuildsWithoutDialing(t *testing.T) {
	// No collector listens here: construction must still succeed (the HTTP
	// exporter connects lazily), and Shutdown flushes nothing fatal.
	for _, ep := range []string{"127.0.0.1:4318", "http://127.0.0.1:4318", "https://127.0.0.1:4318"} {
		tp, err := NewTracerProvider(t.Context(), TracingConfig{
			Endpoint:       ep,
			SampleRatio:    1.0,
			ServiceVersion: "26.0.0.6",
			InstanceID:     "octest",
		})
		if err != nil {
			t.Fatalf("%s: %v", ep, err)
		}
		if err := tp.Shutdown(t.Context()); err != nil {
			t.Errorf("%s: shutdown: %v", ep, err)
		}
	}
}

func TestTracingNilProviderPassthrough(t *testing.T) {
	called := false
	h := Tracing(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
	if !called || rw.Code != http.StatusOK {
		t.Errorf("nil provider must pass through, called=%v code=%d", called, rw.Code)
	}
}
