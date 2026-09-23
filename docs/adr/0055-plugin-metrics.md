# ADR-0055: Phase 4i plugin host-call Prometheus metrics

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

The WASM plugin ABI spec (docs/specs/wasm-plugin-abi.md §12) requires, per
host call, a Prometheus counter
`ncgo_plugin_host_calls_total{plugin, function, result}`, a latency histogram
`ncgo_plugin_host_call_duration_seconds{plugin, function}`, and capability
denial events surfaced as a security signal — plus OTel spans and a
per-plugin admin dashboard.

The project-wide constraint is stdlib-first: ncgo ships its own HTTP router
and OCS renderer and carries no metrics client in go.mod. Adding
`github.com/prometheus/client_golang` (plus its transitive tree) for three
metric families was explicitly rejected. The Prometheus text exposition
format v0.0.4 is simple line-oriented text; a minimal registry is a few
hundred lines of stdlib Go.

## Decision

1. **Stdlib-only registry in `internal/observability`.** `Registry` supports
   counters and fixed-bucket histograms keyed by label set, with families
   registered up front (name, HELP text, canonical label names) and series
   created on first use. `Render(w)` emits text format v0.0.4 — HELP/TYPE
   lines for every family even with zero series (so a fresh scrape never
   looks broken), label-value escaping (`\`, `"`, newline), cumulative
   `*_bucket{le="..."}` lines plus `*_sum` / `*_count`. A single mutex
   guards all state; the race detector covers concurrent hammering. Buckets
   are fixed at
   `{.0005, .001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}` seconds,
   spanning µs host calls to slow outbound I/O. The three §12 families are
   pre-registered by `NewRegistry`.

2. **Bounded `result` classification.** Raw ABI error codes are never label
   values — the label set is the cardinality budget.
   `observability.ClassifyResult` maps codes to a fixed class set: `ok`
   (zero *and* positive values — the ABI contract is negative = error,
   non-negative = success, and several functions return byte counts),
   `permission_denied`, `invalid_argument`, `not_found`, `unavailable`,
   `unsupported`, `too_large`, `timeout`, `canceled`, and `internal` for
   everything else (`internal`, `already_exists`, `quota_exceeded`,
   `conflict`, unknown codes).

3. **Uniform instrumentation at the single choke point.** Every ncgo host
   function (~39) is exported through one `export(name, fn)` helper in
   `registerHostModule`; the helper now passes each function through
   `wrapHostMetrics`. Because all host functions share one uniform shape —
   `func(context.Context, api.Module, ...int32|int64)` returning a single
   `int32` or `int64` — one `reflect.MakeFunc` wrapper covers all of them,
   with the shape validated once at registration time (startup panic on a
   non-conforming function, never per call). Thirteen distinct concrete
   signatures made typed per-shape wrappers the worse trade. The wrapper
   reads the plugin id from the call context (`ctxPluginID`, falling back to
   `"unknown"`), times the call, unpacks `int64` results per the `packI64`
   convention (error code in the high 32 bits), increments
   `ncgo_plugin_host_calls_total{plugin,function,result}`, observes the
   duration histogram, and increments
   `ncgo_plugin_capability_denials_total{plugin,function}` when the result
   is `permission_denied`. A nil registry (`HostConfig.Metrics == nil`)
   registers functions completely unwrapped — zero overhead, not even an
   if-guard per call.

4. **Config-gated endpoint with optional bearer auth.** New config
   `observability.metrics_enabled` (default **false**) and
   `observability.metrics_token` (default empty). When enabled, `app.New`
   constructs one `Registry`, injects it into `plugins.HostConfig`, and
   `mountRoutes` registers `GET /metrics` (exact path) serving
   `text/plain; version=0.0.4; charset=utf-8`. When a token is configured,
   requests must carry `Authorization: Bearer <token>`; the scheme is
   mandatory and the comparison is constant-time over SHA-256 digests (so
   neither the token nor its length leaks through timing), else 401. When
   disabled the route is simply absent (404). Metrics expose per-plugin call
   counts and can reveal plugin inventory, so network-level restriction of
   the listener is recommended regardless of the token.

## Consequences

- Zero new dependencies; `go mod tidy` is a no-op.
- Plugin host calls gain one mutex-protected map update pair (counter +
  histogram) per call when metrics are enabled; uninstrumented hosts are
  byte-identical to before.
- The registry is generic enough for future non-plugin families (jobs,
  preview cache, DAV) via `RegisterCounter` / `RegisterHistogram`.

## Deferred

- ~~**OTel spans** (`plugin.host_call` with plugin.id/abi.version/function/
  error attrs): no OTel SDK in v1, consistent with the stdlib-first
  constraint. The result-classification and label scheme here is the future
  span-attribute mapping.~~ **Resolved by ADR-0072** (Phase 4z, 2026-09-23):
  the OTel SDK landed by explicit user approval; host calls now emit
  `plugin.host.<function>` spans with `plugin.id` / `ncgo.function` /
  `ncgo.result_code` attributes — the mapping this label scheme predicted —
  and requests emit router-named server spans.
- **Per-plugin admin dashboard** (request count, error rate, p50/p95/p99,
  memory high-water, denial events): a UI concern; the Prometheus families
  above already carry the data it needs. Memory high-water marks require
  wazero memory introspection not yet wired.
- **Guest entry-point call metrics** (`Plugin.call` latency/errors): only
  host calls are instrumented in this increment.
- The pre-existing `observability.metrics_listen` (a separate listener
  address) and `observability.otel_endpoint` config keys were never read by
  any code and were **removed** in Phase 4k (2026-09-22) — defined-but-dead
  keys are silent no-ops for operators. `/metrics` is served on the main
  listener behind the token; both keys return when the separate listener
  and the OTel SDK land.
  - `observability.otel_endpoint`: **returned in Phase 4z** (2026-09-23,
    ADR-0072), joined by `observability.otel_sample_ratio`.
  - ~~`observability.metrics_listen`: still deferred.~~ **Resolved by
    ADR-0076** (Phase 5d, 2026-09-23): the key returned — when set,
    `/metrics` is served only on a dedicated listener at that address
    (move, not copy), and `metrics_enabled` is mandatory with it.
