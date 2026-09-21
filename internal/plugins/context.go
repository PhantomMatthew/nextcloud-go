package plugins

import (
	"context"
	"time"
)

// CallContext carries per-request metadata exposed to the plugin through the
// ctx_* host functions.
type CallContext struct {
	UserID    string
	RequestID string
	Locale    string
	Deadline  time.Time
}

type ctxKey int

const (
	ctxPluginID ctxKey = iota
	ctxPluginVersion
	ctxCall
)

// callInfo bundles the plugin being called with its per-call metadata.
type callInfo struct {
	plugin *Plugin
	call   CallContext
}

func withPlugin(ctx context.Context, id, version string) context.Context {
	ctx = context.WithValue(ctx, ctxPluginID, id)
	return context.WithValue(ctx, ctxPluginVersion, version)
}

// WithCallContext attaches per-call metadata for the next Plugin.Call.
func WithCallContext(ctx context.Context, cc CallContext) context.Context {
	return context.WithValue(ctx, ctxCall, cc)
}

func withCall(ctx context.Context, p *Plugin) context.Context {
	var cc CallContext
	if v, ok := ctx.Value(ctxCall).(CallContext); ok {
		cc = v
	}
	ctx = withPlugin(ctx, p.manifest.Plugin.ID, p.manifest.Plugin.Version)
	return context.WithValue(ctx, ctxCall, callInfo{plugin: p, call: cc})
}

func callFromCtx(ctx context.Context) callInfo {
	var info callInfo
	if v, ok := ctx.Value(ctxCall).(callInfo); ok {
		info = v
	}
	return info
}
