# ADR-0076: Phase 5d dedicated metrics listener

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0055 (resolves its last deferred item)

## Context

ADR-0055 (Phase 4i) shipped the stdlib-only Prometheus registry and mounted
`GET /metrics` on the main HTTP listener behind an optional bearer token,
recommending network-level restriction of the listener regardless — metrics
expose per-plugin call counts and reveal plugin inventory. It also pinned a
deferred item: a *separate listener address* for metrics. Phase 4k then
removed the never-read `observability.metrics_listen` key as
defined-but-dead (operators could set it and nothing happened), with the
recorded intent that it return when the separate listener lands. This is
that increment, and it resolves ADR-0055's last deferred bullet.

The operational need is unchanged: operators want to bind the metrics
endpoint to localhost or an inner interface while the main listener is
public, so scrape access is restricted at the network layer rather than by
the bearer token alone.

Two facts from the existing code shape the design:

- `Registry.Handler(token)` (internal/observability/metrics.go) is already
  a self-contained `http.Handler` — bearer auth via constant-time SHA-256
  comparison plus `Render` — reusable as-is on any mux; nothing about it is
  tied to the main router.
- `httpx.Server.Run(ctx)` blocks, shuts down gracefully when ctx is done,
  and fills zero-value timeouts with sane defaults, so a second server is a
  construction call plus a goroutine, not new machinery.

## Decision

1. **The `observability.metrics_listen` key returns** (default `""`), parsed
   into `ObservabilityConfig.MetricsListen` and registered in
   `defaults.go`. Validation (validate.go) requires, when the key is
   non-empty: (a) it parses as `net.SplitHostPort` — an empty host like
   `:9090` is allowed and means all interfaces, matching `server.listen`
   semantics; and (b) `observability.metrics_enabled` is **true**. A
   listener without metrics would serve nothing — precisely the
   defined-but-dead silent no-op Phase 4k eliminated — so the combination
   is a startup-fatal config error that says so plainly, not a quiet
   ignore. `TestLoadUnknownKeysIgnored` drops the key from its
   silently-ignored list (it is live again, exactly as
   `observability.otel_endpoint` left that list in Phase 4z), and
   `testdata/full.yaml` gains the pair.

2. **Move, not copy.** When `metrics_listen` is empty, behavior is
   byte-identical to today: `/metrics` on the main router when metrics are
   enabled. When it is set, `/metrics` is served **only** on the dedicated
   listener; the main-router mount is skipped
   (`if a.metrics != nil && a.Cfg.Observability.MetricsListen == ""`).
   Serving both was rejected: the separate listener exists precisely for
   network-level restriction, and a copy on the public main listener would
   defeat it — the endpoint would remain reachable from every network the
   main listener is, with only the token between the metrics and the
   internet. The bearer token still applies on the dedicated listener (one
   `Registry.Handler(token)` serves both worlds).

3. **Dedicated-listener scope is metrics only.** The mux is a stdlib
   `http.NewServeMux` with the single Go 1.22+ pattern `"GET /metrics"`:
   every other path is 404 and every other method on `/metrics` is 405,
   both for free from the pattern. No `/healthz`, no `/status`, no pprof —
   anything beyond the scrape endpoint is out of scope and would widen the
   restricted listener's attack surface for no operational gain.

4. **Lifecycle: second `httpx.Server`, one cancellable context,
   errors.Join.** `App.Run` builds the main server as before, then asks the
   new testable helper `a.metricsServer()` for the dedicated server (nil
   when metrics are disabled or `metrics_listen` is empty — the
   single-server fast path returns immediately, keeping today's exact
   behavior). With a dedicated server, `Run` derives
   `ctx, cancel := context.WithCancel(ctx)`, runs the metrics server in a
   goroutine reporting to a buffered error channel, and runs the main
   server in the foreground as today. If the metrics server fails (e.g. a
   bind error), the goroutine calls `cancel()` so the main server shuts
   down gracefully, and the metrics error is what `Run` returns. On normal
   shutdown (parent ctx done) the main `Run` returns first; `Run` then
   cancels (a no-op for an already-done parent), collects the metrics
   goroutine's result, and returns `errors.Join` of the two. The dedicated
   listener logs its address at startup through the same
   `httpx.Server` "http server starting" line as the main one. Both servers
   share the derived context, so one parent cancellation drains both
   gracefully within each server's shutdown timeout.

## Alternatives Considered

### Serve /metrics on both listeners
- Pros: a scrape works against either address; zero client reconfiguration
  for deployments that already scrape the main listener.
- Cons: defeats the entire point — network-level restriction of the
  metrics endpoint — by keeping it on the public listener. Operators who
  want both can simply not set `metrics_listen`. Rejected (Decision 2).

### Second listener, full main router
- Pros: no new mux; every endpoint available internally.
- Cons: the whole application surface (DAV, OCS, login, admin) on what is
  meant to be a restricted management listener — a much larger target than
  the one read-only handler that is needed. Rejected (Decision 3).

### net.Listen + http.Serve inline instead of httpx.Server
- Pros: marginally fewer moving parts.
- Cons: re-implements graceful shutdown, shutdown timeouts, and the
  startup log line that `httpx.Server` already provides, for a server that
  must behave identically to the main one under signals. Rejected; reuse
  keeps the lifecycle semantics uniform and tested.

## Consequences

- Zero new dependencies; `go mod tidy` is a no-op.
- `metrics_listen` empty costs exactly nothing: the `metricsServer()` nil
  check keeps the single-server path byte-identical to before, and the
  main-router mount is untouched.
- A failed metrics bind is startup-fatal (via the cancel-and-join
  lifecycle) rather than a silently degraded deployment where scrapes
  mysteriously time out — fail fast beats fail silent for a
  monitoring-adjacent surface.
- ADR-0055's deferred list is now empty; the `metrics_listen` bullet is
  struck through as resolved by this ADR.

## Verification

- `internal/config`: `metrics_listen` parses into the struct via Load
  overrides; validation accepts `127.0.0.1:9090` and `:9090` with
  `metrics_enabled`, rejects `not-an-addr` and a missing port, and rejects
  `metrics_listen` with `metrics_enabled: false`;
  `TestLoadUnknownKeysIgnored` updated and green; `testdata/full.yaml`
  asserts the pair.
- `internal/app` (handler level, no port binding): with metrics enabled
  and `metrics_listen` set, the main router 404s `/metrics` while the
  dedicated mux returns 200 with the right token, 401 without, 404 for
  other paths, and 405 for `POST /metrics`; with `metrics_listen` empty,
  `/metrics` stays on the main router and `metricsServer()` is nil
  (regression guard for today's behavior). A manual real-bind smoke run
  additionally exercised `App.Run` end-to-end: dedicated listener serving,
  main listener 404, graceful parent-cancel shutdown returning nil, and a
  metrics-port bind failure surfacing as `Run`'s error.
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` a no-op,
  `go test -race ./internal/app/ ./internal/config/ ./internal/observability/`
  green.

## References

- ADR-0055 (plugin metrics; its last deferred item resolved here)
- ADR-0072 (OTel spans; the Phase 4z precedent for a removed 4k key
  returning with its feature)
