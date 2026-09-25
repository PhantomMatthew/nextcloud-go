# ADR-0088: Phase 5o guest entry-point call metrics

- **Status**: Accepted
- **Date**: 2026-09-25
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0055 (closes its guest entry-point call metrics deferral)

## Context

ADR-0055 shipped the plugin Prometheus surface with an explicit Deferred
item: **guest entry-point call metrics** (`Plugin.call` latency/errors) —
only host calls (`ncgo.*` imports invoked by the guest) are instrumented in
that increment. The gap is real: an operator watching
`ncgo_plugin_host_calls_total` sees what a plugin asked the host to do, but
not what the host asked the plugin to do. Entry-point invocations —
`Plugin.Call` from dispatchers and `Start`'s ABI check, plus the
install/upgrade/uninstall lifecycle hooks — have no latency or error
visibility at all, so a slow or trapping `ncgo_on_request` handler is
indistinguishable from a fast one until something downstream fails.

Constraints carried over from ADR-0055:

- stdlib-only registry, Prometheus text format v0.0.4; no client_golang
  dependency.
- Label cardinality is the budget: label values come from bounded sets,
  never raw error codes or free-form strings.
- Nil registry means zero added overhead — the uninstrumented posture must
  stay byte-identical for hosts without metrics.

## Decision

1. **Two new families, registered by `NewRegistry`.**
   `ncgo_plugin_entry_calls_total{plugin,entry,result}` (counter:
   "Per-plugin guest entry-point invocations by result class.") and
   `ncgo_plugin_entry_call_duration_seconds{plugin,entry}` (histogram with
   the shared fixed buckets: "Per-plugin guest entry-point latency in
   seconds."). They mirror the host-call pair and render HELP/TYPE lines
   even with zero series.

2. **Result classes for the `result` label.** `ok` on nil error;
   `timeout` when `errors.Is(err, context.DeadlineExceeded)` — checked
   **first**, before trap, because `wrapTrap` double-wraps with
   `%w: %w` so a timed-out call matches both `ErrTrap` and
   `context.DeadlineExceeded`, and the deadline is the more useful signal;
   `trap` on `errors.Is(err, ErrTrap)`; `missing_export` on
   `errors.Is(err, ErrMissingExport)`; `error` for anything else, including
   `manager.acquire` failures. The classification runs on the Go error
   `Plugin.call` returns, pre-interpretation.

3. **Instrumentation at `Plugin.call`, gated on a non-nil registry.**
   `call` is the single funnel for `Call`, `callEntry`, and the lifecycle
   hooks, so wrapping it covers every entry-point invocation — including
   raw `Call` users such as `Start`'s `ncgo_abi_version` check. The body
   moves to an inner func (`callInner`); the outer wrapper branches on
   `p.host.cfg.Metrics == nil` and calls the inner func untouched when
   uninstrumented (the zero-overhead house posture, same as
   `wrapHostMetrics`), otherwise times the call and records both families
   on the single exit. Labels: `plugin` = `p.manifest.Plugin.ID`, `entry` =
   the requested export name, `result` per §2. The nil-plugin guard runs
   before any registry access and records nothing (there is no plugin
   identity to label). **Label cardinality**: entry names are host-requested
   exports declared in the plugin manifest, so the `entry` label space is
   bounded by the manifest, not by network input.

## Alternatives Considered

### Instrumenting at `callEntry` only
- Pros: one fewer wrapper layer; `callEntry` already converts results to
  `PluginError`.
- Cons: misses raw `Call` users — `Start`'s `ncgo_abi_version`, route/event
  dispatch, and every direct entry invocation would stay invisible, which is
  most of the interesting traffic. Rejected.

### Deriving the result class from `PluginError.Code`
(reusing `ClassifyResult(code)` like the host-call wrapper)
- Pros: one shared classifier.
- Cons: `call` sees Go errors before any i32 interpretation — traps,
  timeouts, missing exports, and acquire failures never become
  `PluginError`s (that conversion happens in `callEntry`, one layer up, and
  only for successful calls with non-zero results). Classifying at the code
  level would file every trap under `internal` and lose the
  timeout/trap/missing_export distinctions entirely. Rejected.

## Consequences

- Operators gain per-entry latency and error-rate visibility: trapping or
  slow handlers, timing-out plugins, and misconfigured manifests
  (`missing_export`) are now directly observable per plugin.
- Every instrumented guest call pays one `time.Now` pair and two series
  updates; uninstrumented hosts (nil registry) keep the exact pre-5o call
  path.
- `Start`/`Install`/`Upgrade`/`Uninstall` emit their own series
  (`ncgo_abi_version`, `ncgo_on_install`, …) — expected, and what makes
  boot-time plugin failures visible; dashboards must not assume only
  manifest entry points appear.

## Verification

- `internal/plugins/guest_metrics_test.go`: with a registry wired into the
  test host, a direct `Call("bump")` records
  `ncgo_plugin_entry_calls_total{entry="bump",plugin="com.example.probe",result="ok"} 1`
  plus a duration count of 1, and the install path's own
  `ncgo_abi_version`/`ncgo_on_install` calls appear with `result="ok"`;
  calling an entry the module does not export records `missing_export`; a
  trapping export (wasmgen unreachable probe) records `trap`; a looping
  entry under a 50 ms deadline records `timeout` — never `trap` — with the
  returned error matching both `ErrTrap` and `context.DeadlineExceeded`;
  and a nil-registry host runs the full Load/Install/Call flow panic-free
  recording nothing.
- `internal/observability/metrics_test.go`: render-format assertions pin
  both new families' HELP/TYPE lines and one labeled series each; the
  zero-series render covers them too.
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` clean (no new
  dependencies).
