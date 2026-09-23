package plugins

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

func tracingRecorder(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() {
		// Background, not t.Context(): the test context is already canceled
		// when cleanups run, and Shutdown needs a live one.
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("provider shutdown: %v", err)
		}
	})
	return tp, sr
}

func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("span %q not found among %d ended spans", name, len(spans))
	return nil
}

func spanAttr(t *testing.T, s sdktrace.ReadOnlySpan, key string) string {
	t.Helper()
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	t.Fatalf("attribute %q missing on span %q", key, s.Name())
	return ""
}

// statusCode reads a span's status code, tolerating a nil span for failure
// messages.
func statusCode(s sdktrace.ReadOnlySpan) codes.Code {
	if s == nil {
		return codes.Unset
	}
	return s.Status().Code
}

func TestWrapHostTracingSpanAndParentage(t *testing.T) {
	tp, sr := tracingRecorder(t)
	h := &Host{cfg: HostConfig{TracerProvider: tp}}
	dummy := func(_ context.Context, _ api.Module, v int32) int32 { return v }
	wrapped, ok := h.wrapHostTracing("dummy_fn", dummy).(func(context.Context, api.Module, int32) int32)
	if !ok {
		t.Fatal("wrapped function changed signature")
	}
	// Simulate a request span enclosing the host call: invokeEntry derives
	// the host-call context from the request context, so parentage must hold.
	tracer := tp.Tracer("test")
	ctx, reqSpan := tracer.Start(withPlugin(context.Background(), "com.example.p", "1.0"), "GET /ocs/v2.php/apps/probe")
	if got := wrapped(ctx, nil, pluginsdk.ErrCodeOK); got != pluginsdk.ErrCodeOK {
		t.Fatalf("wrapped call returned %d", got)
	}
	reqSpan.End()

	host := findSpan(t, sr.Ended(), "plugin.host.dummy_fn")
	if got := spanAttr(t, host, "plugin.id"); got != "com.example.p" {
		t.Errorf("plugin.id = %q", got)
	}
	if got := spanAttr(t, host, "ncgo.function"); got != "dummy_fn" {
		t.Errorf("ncgo.function = %q", got)
	}
	if got := spanAttr(t, host, "ncgo.result_code"); got != "0" {
		t.Errorf("ncgo.result_code = %q", got)
	}
	if host.Status().Code != codes.Unset {
		t.Errorf("ok result status = %v, want Unset", host.Status().Code)
	}
	if host.SpanContext().TraceID() != reqSpan.SpanContext().TraceID() {
		t.Error("host span is not in the request trace")
	}
	if host.Parent().SpanID() != reqSpan.SpanContext().SpanID() {
		t.Errorf("host span parent = %q, want request span %q",
			host.Parent().SpanID(), reqSpan.SpanContext().SpanID())
	}
}

func TestWrapHostTracingStatusSemantics(t *testing.T) {
	tp, sr := tracingRecorder(t)
	h := &Host{cfg: HostConfig{TracerProvider: tp}}
	dummy := func(_ context.Context, _ api.Module, code int32) int32 { return code }
	wrapped, ok := h.wrapHostTracing("status_fn", dummy).(func(context.Context, api.Module, int32) int32)
	if !ok {
		t.Fatal("wrapped function changed signature")
	}
	ctx := withPlugin(context.Background(), "com.example.p", "1.0")
	// Only ErrCodeInternal is an error; denials are expected ABI outcomes.
	if wrapped(ctx, nil, pluginsdk.ErrCodeInternal) != pluginsdk.ErrCodeInternal {
		t.Fatal("internal code not passed through")
	}
	if wrapped(ctx, nil, pluginsdk.ErrCodePermissionDenied) != pluginsdk.ErrCodePermissionDenied {
		t.Fatal("denial code not passed through")
	}
	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(spans))
	}
	byCode := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	for _, s := range spans {
		byCode[spanAttr(t, s, "ncgo.result_code")] = s
	}
	internal, okInternal := byCode["-1"]
	denied, okDenied := byCode["-3"]
	if !okInternal || internal.Status().Code != codes.Error {
		t.Errorf("ErrCodeInternal span status = %v, want Error", statusCode(internal))
	}
	if !okDenied || denied.Status().Code != codes.Unset {
		t.Errorf("PermissionDenied span status = %v, want Unset", statusCode(denied))
	}
}

func TestWrapHostTracingInt64Unpack(t *testing.T) {
	tp, sr := tracingRecorder(t)
	h := &Host{cfg: HostConfig{TracerProvider: tp}}
	dummy := func(_ context.Context, _ api.Module) int64 {
		return packI64(pluginsdk.ErrCodeInternal, 7)
	}
	wrapped, ok := h.wrapHostTracing("dummy64", dummy).(func(context.Context, api.Module) int64)
	if !ok {
		t.Fatal("wrapped function changed signature")
	}
	if got := wrapped(context.Background(), nil); got != packI64(pluginsdk.ErrCodeInternal, 7) {
		t.Fatalf("wrapped call returned %d", got)
	}
	s := findSpan(t, sr.Ended(), "plugin.host.dummy64")
	if got := spanAttr(t, s, "ncgo.result_code"); got != "-1" {
		t.Errorf("ncgo.result_code = %q, want packed -1", got)
	}
	if s.Status().Code != codes.Error {
		t.Errorf("packed internal status = %v, want Error", s.Status().Code)
	}
	// No plugin identity in ctx: the "unknown" fallback applies.
	if got := spanAttr(t, s, "plugin.id"); got != "unknown" {
		t.Errorf("plugin.id = %q, want unknown", got)
	}
}

func TestWrapHostTracingNilProviderPassthrough(t *testing.T) {
	h := &Host{}
	dummy := func(_ context.Context, _ api.Module, v int32) int32 { return v }
	wrapped := h.wrapHostTracing("dummy_fn", dummy)
	fn, ok := wrapped.(func(context.Context, api.Module, int32) int32)
	if !ok {
		t.Fatal("signature changed")
	}
	if got := fn(context.Background(), nil, 42); got != 42 {
		t.Errorf("nil provider must pass through, got %d", got)
	}
}
