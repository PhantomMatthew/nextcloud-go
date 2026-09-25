package plugins

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// metricsUnknownPlugin is the plugin label value used when a host call
// carries no plugin identity in its context (for example direct host-function
// invocations outside a Plugin.Call).
const metricsUnknownPlugin = "unknown"

var (
	contextType = reflect.TypeFor[context.Context]()
	moduleType  = reflect.TypeFor[api.Module]()
)

// wrapHostMetrics instruments one ncgo host function with the §12 metric
// families: a call counter labeled by plugin/function/result class, a latency
// histogram by plugin/function, and a capability-denial counter when the call
// is rejected with ErrCodePermissionDenied. A nil registry returns fn
// untouched so uninstrumented hosts pay zero overhead.
//
// The wrapper uses reflection because every registered host function shares
// one uniform shape — func(context.Context, api.Module, ...int32|int64) with
// a single int32 or int64 result — so a single reflect.MakeFunc covers all
// of them; the shape is validated at registration time (startup), never per
// call. int64 results unpack the error code from the high 32 bits per the
// packI64 convention.
func (h *Host) wrapHostMetrics(name string, fn any) any {
	reg := h.cfg.Metrics
	if reg == nil {
		return fn
	}
	v := reflect.ValueOf(fn)
	t := v.Type()
	assertHostFuncShape(name, t)
	return reflect.MakeFunc(t, func(args []reflect.Value) []reflect.Value {
		plugin := metricsUnknownPlugin
		if ctx, ok := args[0].Interface().(context.Context); ok {
			if id, ok := ctx.Value(ctxPluginID).(string); ok && id != "" {
				plugin = id
			}
		}
		start := time.Now()
		out := v.Call(args)
		reg.ObserveHistogram(observability.MetricPluginHostCallDurationSeconds,
			time.Since(start).Seconds(),
			observability.Label{Name: "plugin", Value: plugin},
			observability.Label{Name: "function", Value: name})
		code := int32(out[0].Int()) //nolint:gosec // G115: int32 result widened by reflect
		if t.Out(0).Kind() == reflect.Int64 {
			code = int32(out[0].Int() >> 32) //nolint:gosec // G115: intentional packI64 ABI unpacking (error code in the high 32 bits)
		}
		reg.IncCounter(observability.MetricPluginHostCallsTotal,
			observability.Label{Name: "plugin", Value: plugin},
			observability.Label{Name: "function", Value: name},
			observability.Label{Name: "result", Value: observability.ClassifyResult(code)})
		if code == pluginsdk.ErrCodePermissionDenied {
			reg.IncCounter(observability.MetricPluginCapabilityDenialsTotal,
				observability.Label{Name: "plugin", Value: plugin},
				observability.Label{Name: "function", Value: name})
		}
		return out
	}).Interface()
}

// recordEntryCall records one guest entry-point invocation in the §12
// entry-call families: a latency observation and a call counter labeled by
// plugin/entry/result class. Callers gate on a non-nil registry, so this
// never runs on the zero-overhead uninstrumented path.
func (p *Plugin) recordEntryCall(entry string, start time.Time, err error) {
	reg := p.host.cfg.Metrics
	plugin := p.manifest.Plugin.ID
	reg.ObserveHistogram(observability.MetricPluginEntryCallDurationSeconds,
		time.Since(start).Seconds(),
		observability.Label{Name: "plugin", Value: plugin},
		observability.Label{Name: "entry", Value: entry})
	reg.IncCounter(observability.MetricPluginEntryCallsTotal,
		observability.Label{Name: "plugin", Value: plugin},
		observability.Label{Name: "entry", Value: entry},
		observability.Label{Name: "result", Value: classifyCallResult(err)})
}

// classifyCallResult maps a guest call error to its bounded result-class
// label value. DeadlineExceeded is checked first: wrapTrap double-wraps the
// underlying error with %w, so a call that timed out matches both ErrTrap
// and context.DeadlineExceeded, and the timeout is the more useful signal.
// Anything else — acquire failures included — collapses to "error".
func classifyCallResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrTrap):
		return "trap"
	case errors.Is(err, ErrMissingExport):
		return "missing_export"
	default:
		return "error"
	}
}

// countStorageBytes records n actually-transferred storage bytes in the §12
// bytes family, labeled by calling plugin, operation, and scope. A nil
// registry is zero overhead (the 4i posture), and the plugin id follows the
// 4n/4o nil-guard: no call context means an empty id, which is still
// counted.
func (h *Host) countStorageBytes(ctx context.Context, op, scope string, n int64) {
	reg := h.cfg.Metrics
	if reg == nil {
		return
	}
	var plugin string
	if info := callFromCtx(ctx); info.plugin != nil {
		plugin = info.plugin.manifest.Plugin.ID
	}
	reg.AddCounter(observability.MetricPluginStorageBytesTotal, n,
		observability.Label{Name: "plugin", Value: plugin},
		observability.Label{Name: "op", Value: op},
		observability.Label{Name: "scope", Value: scope})
}

// assertHostFuncShape panics at registration time unless fn is
// func(context.Context, api.Module, ...int32|int64) (int32|int64) — the only
// shapes registerHostModule exports.
func assertHostFuncShape(name string, t reflect.Type) {
	ok := t.Kind() == reflect.Func && t.NumIn() >= 2 && t.NumOut() == 1 &&
		t.In(0) == contextType && t.In(1) == moduleType &&
		(t.Out(0).Kind() == reflect.Int32 || t.Out(0).Kind() == reflect.Int64)
	if ok {
		for i := 2; i < t.NumIn(); i++ {
			if k := t.In(i).Kind(); k != reflect.Int32 && k != reflect.Int64 {
				ok = false
				break
			}
		}
	}
	if !ok {
		panic("plugins: host function " + name + " has unsupported signature " + t.String())
	}
}
