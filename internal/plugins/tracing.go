package plugins

import (
	"context"
	"reflect"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// wrapHostTracing wraps one ncgo host function in a plugin.host.<name> span
// with plugin.id, ncgo.function, and ncgo.result_code attributes. Only
// ErrCodeInternal marks the span Error: capability denials and other
// classified results are expected ABI outcomes (the metrics layer counts
// them separately). A nil provider returns fn untouched so untraced hosts
// pay zero overhead.
//
// The span is a child of whatever trace the calling context carries —
// invokeEntry derives the host-call context from the request context, so the
// request span → host-call span parentage needs no extra plumbing. The same
// reflection shape as wrapHostMetrics applies; it is validated at
// registration time.
func (h *Host) wrapHostTracing(name string, fn any) any {
	tp := h.cfg.TracerProvider
	if tp == nil {
		return fn
	}
	tracer := tp.Tracer("github.com/PhantomMatthew/nextcloud-go/internal/plugins")
	v := reflect.ValueOf(fn)
	t := v.Type()
	assertHostFuncShape(name, t)
	return reflect.MakeFunc(t, func(args []reflect.Value) []reflect.Value {
		ctx, ok := args[0].Interface().(context.Context)
		if !ok || ctx == nil {
			ctx = context.Background()
		}
		plugin := metricsUnknownPlugin
		if id, ok := ctx.Value(ctxPluginID).(string); ok && id != "" {
			plugin = id
		}
		ctx, span := tracer.Start(ctx, "plugin.host."+name,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(
				attribute.String("plugin.id", plugin),
				attribute.String("ncgo.function", name),
			))
		args[0] = reflect.ValueOf(ctx)
		out := v.Call(args)
		code := int32(out[0].Int()) //nolint:gosec // G115: int32 result widened by reflect
		if t.Out(0).Kind() == reflect.Int64 {
			code = int32(out[0].Int() >> 32) //nolint:gosec // G115: intentional packI64 ABI unpacking (error code in the high 32 bits)
		}
		span.SetAttributes(attribute.Int("ncgo.result_code", int(code)))
		if code == pluginsdk.ErrCodeInternal {
			span.SetStatus(codes.Error, "host call internal error")
		}
		span.End()
		return out
	}).Interface()
}
