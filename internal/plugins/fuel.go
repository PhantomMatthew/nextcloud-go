package plugins

import (
	"context"
	"fmt"
	"reflect"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
)

// fuel.go enforces the manifest's runtime.fuel_per_call as a wasm
// function-entry budget per plugin call (ADR-0103): guest function entries
// fire a compile-time experimental.FunctionListener shared across the
// plugin's instances, ncgo host-function entries are counted by the
// registration-time wrapHostFuel wrapper, and both funnel into the per-call
// meter found through the calling module instance. Exceeding the budget
// panics with ErrFuelExhausted; wazero's call recovery wraps it with %w, and
// the call sites map the trip back via the meter (never wrapTrap) and destroy
// the instance exactly like a trap.
//
// The unit is function calls, not instructions: wazero v1.12.0 has no
// instruction-fuel API anywhere (upstream issue wazero/wazero#422 is open),
// so this is an interim approximation. A tight loop inside one function makes
// no calls and stays bounded only by cpu_timeout_ms.

// callMeter counts wasm function entries during one plugin call against the
// plugin's fuel_per_call budget. Calls on one instance are serialized by
// construction (pool checkout / singleMu / fresh per-request instance), and
// the listener and host wrapper run on the calling goroutine, so the counter
// needs no synchronization of its own.
type callMeter struct {
	budget  uint64
	count   uint64
	tripped bool
}

// reset re-arms the meter for the next call on the instance.
func (m *callMeter) reset() {
	m.count, m.tripped = 0, false
}

// exceeded reports whether the meter tripped its budget.
func (m *callMeter) exceeded() bool {
	return m.tripped
}

// enter counts one function entry. Past the budget it trips and panics with
// ErrFuelExhausted, which aborts the in-flight call immediately at function
// granularity (verified for the compiler and interpreter engines: the panic
// unwinds the wasm stack and fn.Call returns an error wrapping the sentinel).
// The pinned alternative — mod.CloseWithExitCode from the listener — was
// rejected on test evidence: wazero checks the exit code only at loop
// back-edges, so a call-only recursion of 1e6 entries ran to completion
// (1,000,001 entries) after the module was closed at entry 51.
func (m *callMeter) enter() {
	if m.tripped {
		panic(ErrFuelExhausted)
	}
	m.count++
	if m.count > m.budget {
		m.tripped = true
		panic(ErrFuelExhausted)
	}
}

// fuelListenerFactory returns the FunctionListenerFactory attached to the
// CompileModule context in Load when the manifest sets fuel_per_call: every
// defined guest function shares one listener that enters the calling
// instance's meter. wazero binds listeners at CompileModule (attaching the
// factory at InstantiateModule is silently ignored) and shares them across
// the module's instances, so the per-instance meter is found through the
// host's module map. The ncgo host module is compiled separately without a
// factory, so host-function entries never fire this listener — they are
// counted by wrapHostFuel instead.
func (h *Host) fuelListenerFactory() experimental.FunctionListenerFactory {
	return experimental.FunctionListenerFactoryFunc(func(api.FunctionDefinition) experimental.FunctionListener {
		return experimental.FunctionListenerFunc(func(_ context.Context, mod api.Module, _ api.FunctionDefinition, _ []uint64, _ experimental.StackIterator) {
			if m := h.fuelMeterFor(mod); m != nil {
				m.enter()
			}
		})
	})
}

// wrapHostFuel counts ncgo host-function entries against the calling
// instance's meter: every exported host function is wrapped at registration
// (the uniform reflect shape, like wrapHostMetrics). A call from a module
// without a meter — fuel_per_call unset, or a host invocation outside a
// plugin call — passes through with one map lookup of overhead; a metered
// call entering past its budget panics before any host work runs.
func (h *Host) wrapHostFuel(name string, fn any) any {
	v := reflect.ValueOf(fn)
	t := v.Type()
	assertHostFuncShape(name, t)
	return reflect.MakeFunc(t, func(args []reflect.Value) []reflect.Value {
		if mod, ok := args[1].Interface().(api.Module); ok {
			if m := h.fuelMeterFor(mod); m != nil {
				m.enter()
			}
		}
		return v.Call(args)
	}).Interface()
}

// registerFuelMeter associates an instance's meter with its module so the
// shared listener and the host wrapper can find it. Called at instantiation
// alongside registerHandles — after _initialize, which therefore stays
// unmetered (bounded by cpu_timeout_ms, exactly as before ADR-0103).
func (h *Host) registerFuelMeter(mod api.Module, m *callMeter) {
	h.fuelMu.Lock()
	defer h.fuelMu.Unlock()
	h.fuelMeters[mod] = m
}

// unregisterFuelMeter drops the association when an instance closes.
func (h *Host) unregisterFuelMeter(mod api.Module) {
	h.fuelMu.Lock()
	defer h.fuelMu.Unlock()
	delete(h.fuelMeters, mod)
}

// fuelMeterFor returns the call meter of the calling module instance, or nil
// for fuel_per_call-unset modules and calls outside a plugin call.
func (h *Host) fuelMeterFor(mod api.Module) *callMeter {
	h.fuelMu.RLock()
	defer h.fuelMu.RUnlock()
	return h.fuelMeters[mod]
}

// fuelKill returns the ErrFuelExhausted error for a call whose instance meter
// tripped, counting the kill in the §12 fuel family (nil registry: no
// metric). It returns nil when the instance is unmetered or the call failed
// for another reason, leaving the caller to wrapTrap as usual.
func (p *Plugin) fuelKill(inst *instance, entry string) error {
	m := inst.fuel
	if m == nil || !m.exceeded() {
		return nil
	}
	id := p.manifest.Plugin.ID
	if reg := p.host.cfg.Metrics; reg != nil {
		reg.IncCounter(observability.MetricPluginFuelExceededTotal,
			observability.Label{Name: "plugin", Value: id})
	}
	return fmt.Errorf("%w: plugin %s entry %s used over %d function entries in one call",
		ErrFuelExhausted, id, entry, m.budget)
}
