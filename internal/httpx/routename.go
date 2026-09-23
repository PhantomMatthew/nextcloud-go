package httpx

import "context"

const ctxKeyRouteName ctxKey = ctxKeyRequestID + 1

// RouteNameFromContext returns the registered route the router matched for
// this request — the exact path for exact routes, the registered prefix for
// prefix routes — or "" when the request has not been routed yet or matched
// nothing. Tracing uses it for low-cardinality span names; it is never the
// raw request path.
func RouteNameFromContext(ctx context.Context) string {
	v, ok := ctx.Value(ctxKeyRouteName).(string)
	if !ok {
		return ""
	}
	return v
}

// withRouteName returns ctx annotated with the matched route name.
func withRouteName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxKeyRouteName, name)
}
