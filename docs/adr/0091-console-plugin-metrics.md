# ADR-0091: Phase 5r per-plugin metrics panel in the admin console

- **Status**: Accepted
- **Date**: 2026-09-25
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0055 (resolves its per-plugin admin dashboard deferral,
  except memory high-water)

## Context

ADR-0055 (Phase 4i plugin host-call metrics) deferred "per-plugin admin
dashboard (request count, error rate, p50/p95/p99, memory high-water, denial
events)". The data has been in place since ADR-0055 and ADR-0088 (guest
entry-point metrics): six pre-registered families covering host calls,
entry calls, latencies, capability denials, and storage bytes. What never
existed is a **read path**: the registry is render-only — `Render(w)` writes
the Prometheus text exposition and nothing reads the series back — so the
admin console (ADR-0080) cannot answer "how is plugin X behaving?" without
an external Prometheus scraping `/metrics`.

Constraints carried over:

- Hand-written HTML/JS/CSS console, no framework, no build step (ADR-0080);
  read-only JSON endpoints behind the Auth + RequireAdmin chain.
- The registry stays stdlib-only (ADR-0055) — no client_golang to get a
  queryable store from.
- All histogram families share one fixed le bucket grid
  (`{.0005, …, 2.5}` seconds, metrics.go), so per-function series for one
  plugin can be merged bucket-by-bucket.
- **Memory high-water stays out of scope**: per-plugin memory high-water
  marks require wazero memory introspection that is still not wired
  (carried verbatim from ADR-0055's deferral). The panel ships without it.

## Decision

1. **Registry read API** (`internal/observability/read.go`):
   `CounterSeries(name)` / `HistogramSeries(name)` return snapshot slices of
   exported read models (`CounterSeries{Labels, Value}`,
   `HistogramSeries{Labels, Buckets, Sum, Count}` with cumulative buckets,
   final bucket `Le=+Inf`). Same mutex discipline as the writers, series
   ordered by key like `Render`, labels canonical name-sorted, and every
   slice a defensive copy — callers never touch registry internals. Unknown
   or series-less families yield nil. `Quantile(h, q)` estimates from
   cumulative buckets with Prometheus `histogram_quantile` semantics
   (rank = q × count, linear interpolation within the matched bucket, a rank
   in the +Inf bucket caps at the last finite le); count 0 or empty yields
   0, and q outside [0,1] clamps rather than answering ±Inf so results stay
   JSON-safe.

2. **Endpoint**: `GET /console/api/plugins`, dispatched like the other
   console APIs. `console.Handler` gains a concrete
   `Metrics *observability.Registry` field (an interface buys nothing — the
   console already imports config/database concretes); nil means metrics are
   disabled and answers **503 `{"error":"metrics disabled"}`** rather than an
   empty table — an empty table would be indistinguishable from "no plugin
   traffic". `writeError` gains a status-code parameter for this (existing
   callers pass 500 unchanged).

3. **Per-plugin merged-bucket aggregation**: one row per plugin id found in
   **any** of the six families (missing families contribute zeros), sorted
   by id. Counters sum over function/result/op/scope; errors are
   `result != "ok"`. Latency stats **merge** the plugin's per-function (or
   per-entry) histogram series first — identical-le cumulative counts, sums,
   and counts add because the grid is fixed — then mean = sum/count (0 when
   count is 0) and p50/p95/p99 come from `Quantile` over the merged series.
   The JSON carries mean/p50/p95/p99 for both host and entry calls; the
   table renders mean + p95 to stay narrow.

4. **UI**: a Plugins section under Notifications in the embedded shell,
   following the notifications view's fetch/table/empty-row/refresh pattern
   (ADR-0080 constraints, no new CSS). A 503 renders "metrics disabled" as
   the section's empty-row message instead of raising the banner.

5. **Wiring**: `mountRoutes` passes `a.metrics` (nil when
   `observability.metrics_enabled=false`) into the console handler.
   `plugins.HostConfig.Metrics` was already wired to the same registry
   (app.go), so host-call and entry metrics were live; only the read side
   was missing.

## Alternatives Considered

### Parsing the Prometheus text render
- Cons: re-parsing our own exposition output to serve JSON is fragile —
  float formatting, escaping, and HELP/TYPE noise become an internal API
  contract; it also forces a parse on every console load. Rejected: the
  registry owns the series; a typed read API is strictly simpler.

### Per-function rows instead of the per-plugin merge
- Pros: finer resolution in the console.
- Cons: the deferred dashboard is per-plugin by design ("how is plugin X
  doing?"), and per-function resolution already exists in the raw `/metrics`
  exposition for whoever needs it. Rejected; merging also keeps the table to
  one row per plugin.

## Consequences

- ADR-0055's dashboard deferral is closed except memory high-water, which
  remains blocked on wazero memory introspection and stays deferred there.
- The registry gains a stable in-process read surface (snapshots +
  `Quantile`) usable by future consumers (CLI, diagnostics) without touching
  the exposition path; Render's output is unchanged.
- Metrics disabled is an explicit operator-visible state (503 + section
  message), not a silent empty view. Zero new dependencies.

## Verification

- `internal/observability/read_test.go`: counter/histogram dumps (canonical
  labels, values, cumulative bucket shape with the +Inf final bucket,
  unknown/histogram/counter family mismatch → empty), defensive copies,
  `Quantile` table (empty, zero-count, midpoint interpolation, q=0/q=1,
  clamped q, last-bucket cap) plus a single-observation registry case.
- `internal/console/console_test.go`: a seeded real registry across three
  plugins (host-only, entry-only, storage-only) aggregates to the pinned
  JSON shape — merged-bucket mean/p50/p95 asserted to the interpolated
  values, zero-family plugin carries zeros; nil `Metrics` → 503 JSON.
- `internal/app/console_route_test.go`: through the real router — metrics
  disabled (default) → admin 503 / anonymous 401; metrics enabled with a
  seeded `a.metrics` → 200 with the aggregated row.
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` clean (no new
  dependencies), `node --check internal/console/ui/console.js`.
