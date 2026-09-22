package plugins

import (
	"context"
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
