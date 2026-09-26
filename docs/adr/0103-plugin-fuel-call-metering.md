# ADR-0103: Plugin fuel enforcement as wasm function-call metering

- **Status**: Accepted
- **Date**: 2026-09-27
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0006 (its fuel deferral is resolved — by approximation),
  ADR-0056 (its "fuel_per_call parsed but unenforced" note)

## Context

`runtime.fuel_per_call` has been parsed since Phase 0
(internal/plugins/manifest.go) but never enforced, recorded as a confirmed
spec deviation in ADR-0056 and the ABI spec Change Log: "wazero v1 has no
fuel-metering API; CPU budget remains wall-clock timeout only". Re-probed
today, that premise stands for **instruction** fuel: wazero v1.12.0 is the
latest release (go.mod pins it) and neither its public nor its experimental
API carries instruction fuel or metering (experimental has only
CloseNotifier, CompilationWorkers, FunctionListenerFactory, ImportResolver,
MemoryAllocator, Snapshotter); upstream issue wazero/wazero#422 (CPU limit)
is still open. A fork, `github.com/thevilledev/wazero`, does have an
`experimental/fuel` package, but adopting it is rejected: the wasm runtime
is the critical-path dependency of the whole plugin system, and pinning it
to a personal fork fails the project's security/maintenance posture and the
zero-new-dependency gate.

What official wazero v1.12.0 *does* offer is
`experimental.WithFunctionListenerFactory`: a listener invoked on every
wasm function entry. That yields an interim approximation of fuel —
**counting function calls instead of instructions** — with no new
dependency.

Two mechanism findings from probe tests (wasmgen `FuelModule`: a
self-recursive `recurse(n)` and a host-call loop `spam(n)` against a
listener that kills at entry 51):

1. **Kill mechanism**: calling `mod.CloseWithExitCode` from inside the
   listener's `Before` does *not* stop the in-flight call at function
   granularity. wazero checks the module exit code only at loop back-edges
   (with `WithCloseOnContextDone`), so a call-only recursion of 1e6 entries
   ran to completion — all 1,000,001 entries in ~90 ms — and only then
   surfaced `module closed with exit_code(42)`. Panicking from `Before`
   with a sentinel `error` value stops the call at exactly entry 51 in
   ~63 µs (compiler engine) / ~137 µs (interpreter), and wazero's call
   recovery wraps it with `%w` ("(recovered by wazero)"), so `errors.Is`
   survives in both engines.
2. **Wiring**: listeners bind at **CompileModule**, not InstantiateModule —
   attaching the factory to the `InstantiateModule` context is silently
   ignored (zero listener invocations measured). The factory's listeners
   are shared by all of the compiled module's instances and receive the
   calling `api.Module`, so a per-instance meter is found through a
   host-level module→meter map. The ncgo host module is compiled
   separately, without a factory, so **host-function entries never fire the
   guest listener** (measured: `spam(5)` fired it once, for the `spam`
   entry itself); host entries are counted by a registration-time wrapper
   instead.

## Decision

1. **Semantics (unit redefinition)**: `fuel_per_call` is now enforced as
   the maximum number of wasm **function entries** — guest functions and
   ncgo host-function entries — allowed during one plugin call
   (`Plugin.call` and everything funnelling through the same
   acquire-and-call sites: entry calls, lifecycle hooks, event/job
   delivery, WebDAV prop getters, HTTP dispatch). The unit changes from
   "instructions" (never enforced) to "function calls"; the spec states
   this loudly. `0` (unset) stays unlimited and compiles without a
   listener factory — the guest path is byte-identical to before.

2. **Mechanism** (`internal/plugins/fuel.go`): a per-instance `callMeter`
   {budget, count, tripped}. `Host.Load` compiles fuel-setting plugins with
   `experimental.WithFunctionListenerFactory` on the compile context; the
   shared listener enters the calling instance's meter, found via the
   host's module→meter map (registered at instantiation, dropped at
   instance close). Every exported ncgo host function is wrapped at
   registration (`wrapHostFuel`, the same uniform reflect shape as
   `wrapHostMetrics`) and enters the same meter. `acquire` re-arms the
   meter on every checkout in all three instance models, so each call gets
   the full budget; calls on one instance are serialized by construction,
   so the counter needs no synchronization. `meter.enter()` panics with
   `ErrFuelExhausted` once count > budget — per finding (1), the only kill
   that stops the call promptly at function granularity.

3. **Error and destruction**: the new sentinel `ErrFuelExhausted`
   (internal/plugins/errors.go) is returned wrapped with plugin id, entry
   name, and budget, and is **never** wrapped in `ErrTrap` — but the
   instance is destroyed with exact trap semantics (`release(true)`:
   pooled close + replenish, singleton cleared, per_request closed). The
   mapping lives at every guest-call site (`callInner`, `invokeEntryMode`,
   `callPropGetter`, `beginRequest`/`callExport`), keyed off the tripped
   meter rather than the error text.

4. **Metrics**: new pre-registered counter
   `ncgo_plugin_fuel_exceeded_total{plugin}` (ADR-0095's registration and
   label pattern), incremented on every fuel kill when cfg.Metrics is
   non-nil; the console plugins panel gains a "Fuel hits" column fed from
   it. §12's entry-call result classes are unchanged (fuel kills count as
   `error` there).

5. **Boundaries deliberately kept**: `_initialize` runs before the meter is
   registered, so toolchain runtime setup stays unmetered (bounded by
   `cpu_timeout_ms` exactly as before); the wazero-bundled WASI functions
   (fd_write & friends) are not ncgo host functions and do not count; a
   fuel trip inside `ncgo_alloc`/`ncgo_free` during argument marshalling
   surfaces as a trap (still destroying the instance) rather than
   ErrFuelExhausted.

## Alternatives Considered

### thevilledev/wazero fork (true instruction fuel)

- Pros: real per-instruction budgets; no approximation gap.
- Cons: the plugin system's critical-path runtime becomes a personal fork —
  supply-chain, maintenance, and bus-factor risk — and violates the
  zero-new-dependency gate. Rejected; revisit if upstream lands #422.

### CloseWithExitCode from the listener

- Pros: no panic in the design; uses a documented "safe for concurrent
  use" API.
- Cons: probe evidence above — exit checks exist only at loop back-edges,
  so call-only recursion blows the budget by orders of magnitude before
  noticing (1,000,001 entries against a budget of 50). Rejected on the
  measured evidence; the panic sentinel kills at the tripping entry.

### Guest-listener-only counting

- Pros: no host-side wrapper at all; fuel=0 path untouched everywhere.
- Cons: a plugin can make unbounded host calls inside one plugin call
  (each host call is uncounted guest-visible work) — the budget stops
  meaning anything for chatty plugins. Rejected.

## Consequences

- **Approximation gap**: the unit is function calls, not instructions. A
  tight loop *inside* one function makes no calls and is bounded only by
  the existing wall-clock `cpu_timeout_ms` (default 5 s) — the residual
  gap until upstream ships instruction fuel. Function-entry granularity
  also means a plugin calling many tiny functions burns budget faster than
  one doing the same work inline; budgets are therefore not comparable
  across guest compilers.
- **Overhead**: fuel=0 compiles without a listener factory — the guest
  path is byte-identical to before. fuel>0 costs one interface call plus a
  map lookup and increment per guest function entry, and the host-function
  wrapper costs one reflect dispatch plus a map lookup per host entry
  (paid by all plugins, since the shared ncgo module is registered once —
  the common unmetered case short-circuits on a nil map entry). Rollback
  is trivial: `fuel_per_call = 0` (or omitting it) restores the previous
  behavior exactly.
- `ErrFuelExhausted` joins the public error surface; callers can
  distinguish resource kills from traps (`errors.Is`), while instance
  destruction keeps trap semantics, so no pool/singleton invariants
  change.
- ADR-0006's "revisit fuel metering in Phase 4" is discharged by
  approximation; ADR-0056's deviation note is struck.

## Verification

- Mechanism probes (temporary, evidence above): CloseWithExitCode vs panic
  sentinel on `recurse(1e6)` with budget 50, both engines; host-entry
  visibility of the guest listener.
- `internal/plugins/fuel_test.go` (wasmgen `FuelModule` through Load +
  Install/Call): fuel=0 unmetered (nil meter, empty host map, 100k-entry
  call succeeds); small budget → `errors.Is(err, ErrFuelExhausted)`, not
  ErrTrap, message carries plugin id/entry/budget; the killed call stops
  promptly (1e6-entry recursion, budget 50, returns in well under 5 s);
  pooled kill destroys and replenishes (next call runs on a fresh
  instance); singleton kill clears and re-instantiates; meter resets per
  call (two consecutive boundary calls succeed); the counter increments
  per kill and nil-metrics kills cleanly; host entries count (spam(4)
  passes budget 5, spam(5) trips); pooled concurrent over/under-budget
  calls keep separate accounting under `-race`.
- `internal/observability`: zero-series render covers the new family.
- `internal/console`: plugins payload test pins `fuel_exceeded` (set,
  absent, zero-family rows); `node --check console.js`.
- Full suite + `go test -race` green; `golangci-lint run ./...` 0 issues;
  `go mod tidy` no diff (wazero experimental is the same module — zero new
  dependencies).
