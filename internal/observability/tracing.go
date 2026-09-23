package observability

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
)

// tracerScope is the instrumentation scope name attached to every span ncgo
// produces.
const tracerScope = "github.com/PhantomMatthew/nextcloud-go/internal/observability"

// TracingConfig carries the operator-facing tracing knobs.
type TracingConfig struct {
	// Endpoint is the OTLP/HTTP collector target: a bare host:port (plaintext
	// HTTP, the common in-cluster collector deployment) or a full http(s)://
	// URL. Empty is rejected here; the app layer skips construction entirely
	// when unconfigured.
	Endpoint string
	// SampleRatio is the head-sampling probability in [0,1], applied
	// parent-based so upstream sampled traces stay whole.
	SampleRatio float64
	// ServiceVersion is the dotted ncgo server version (internal/version).
	ServiceVersion string
	// InstanceID is the oc… instance identifier.
	InstanceID string
}

// parseOTLPEndpoint classifies a configured endpoint. asURL distinguishes a
// full URL (scheme, host, port, and path are honored, and the exporter
// derives TLS from the scheme) from a bare host:port, which always means
// insecure plaintext — collectors typically terminate TLS upstream or run
// plain HTTP in-cluster.
func parseOTLPEndpoint(endpoint string) (asURL, insecure bool, err error) {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return false, false, fmt.Errorf("observability: empty OTLP endpoint")
	}
	if !strings.Contains(ep, "://") {
		return false, true, nil
	}
	u, err := url.Parse(ep)
	if err != nil {
		return false, false, fmt.Errorf("observability: OTLP endpoint: %w", err)
	}
	switch u.Scheme {
	case "http":
		return true, true, nil
	case "https":
		return true, false, nil
	default:
		return false, false, fmt.Errorf("observability: OTLP endpoint scheme %q must be http or https", u.Scheme)
	}
}

// Sampler returns the provider sampler: parent-based (an upstream sampled
// trace is kept whole) over a ratio head sampler.
func Sampler(ratio float64) sdktrace.Sampler {
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

// NewTracerProvider builds an SDK TracerProvider exporting spans to an
// OTLP/HTTP collector via a BatchSpanProcessor. Construction dials nothing —
// the HTTP exporter connects lazily on the first flush — so a down collector
// never blocks startup. The caller owns Shutdown.
func NewTracerProvider(ctx context.Context, cfg TracingConfig) (*sdktrace.TracerProvider, error) {
	asURL, insecure, err := parseOTLPEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	ep := strings.TrimSpace(cfg.Endpoint)
	var opts []otlptracehttp.Option
	if asURL {
		opts = append(opts, otlptracehttp.WithEndpointURL(ep))
	} else {
		opts = append(opts, otlptracehttp.WithEndpoint(ep))
		if insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("observability: OTLP exporter: %w", err)
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName("ncgo"),
		semconv.ServiceVersion(cfg.ServiceVersion),
		semconv.ServiceInstanceID(cfg.InstanceID),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: resource: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(Sampler(cfg.SampleRatio)),
		sdktrace.WithBatcher(exp),
	), nil
}

// Tracing returns a middleware that starts one server span per request and
// ends it when the handler returns. Upstream W3C traceparent headers are
// honored (absent or malformed headers start a new root). Span names stay
// low-cardinality: the name is the bare method until the router's matched
// route (the registered exact path or prefix, carried on the request
// context) turns it into "GET /status.php" — the raw request path only ever
// lands on the http.target attribute, never in the name. 5xx statuses mark
// the span Error; everything else stays Unset. A nil provider passes
// requests through untouched.
func Tracing(tp trace.TracerProvider) httpx.Middleware {
	if tp == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	tracer := tp.Tracer(tracerScope)
	prop := propagation.TraceContext{}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			name := r.Method
			attrs := []attribute.KeyValue{
				attribute.String("http.method", r.Method),
				attribute.String("http.target", r.URL.Path),
			}
			if route := httpx.RouteNameFromContext(ctx); route != "" {
				name = r.Method + " " + route
				attrs = append(attrs, attribute.String("http.route", route))
			}
			if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				attrs = append(attrs, attribute.String("http.client_ip", ip))
			}
			ctx, span := tracer.Start(ctx, name,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attrs...))
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				// Recover (outermost middleware) converts panics to 500s; here
				// the span is marked and ended, then the panic travels on.
				if rec := recover(); rec != nil {
					span.SetStatus(codes.Error, fmt.Sprintf("panic: %v", rec))
					span.End()
					panic(rec)
				}
				span.SetAttributes(attribute.Int("http.status_code", rw.status))
				// RequestID runs downstream, so the id never reaches this
				// request's context; the response header it sets does.
				if id := w.Header().Get(httpx.HeaderRequestID); id != "" {
					span.SetAttributes(attribute.String("request_id", id))
				}
				if rw.status >= http.StatusInternalServerError {
					span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(rw.status))
				}
				span.End()
			}()
			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

// statusRecorder captures the response status code for the span. Handlers
// that only Write produce the net/http implicit 200.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}
