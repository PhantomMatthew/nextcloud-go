# ADR-0072: Phase 4z OpenTelemetry spans — OTLP/HTTP exporter, request & plugin spans

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0055 (resolves its OTel-spans follow-up)

## Context

ADR-0055 deferred OTel spans ("no OTel SDK in v1, consistent with the
stdlib-first constraint"), and Phase 4k removed `observability.otel_endpoint`
as a defined-but-dead key, noting it returns "when the OTel SDK lands". On
2026-09-23 the user explicitly approved introducing the OTel SDK — a scoped
exception to the project-wide zero-dependency posture — covering exactly
three modules and nothing else:

- `go.opentelemetry.io/otel` (API; W3C TraceContext propagation lives in the
  core module's `propagation` package)
- `go.opentelemetry.io/otel/sdk` (TracerProvider, samplers, and the
  `sdk/trace/tracetest` in-memory recorder used by every test here)
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`
  (OTLP/HTTP exporter)

Contrib (`otelhttp` et al.) and the gRPC exporter remain forbidden.

## Decision

### Version pinning: v1.44.0, not @latest

`go get ...@latest` (v1.46.0) dragged the whole module graph forward
(grpc-gateway v2.27.1→v2.30.0, grpc v1.82→v1.83, genproto, protobuf,
go-logr) and, through the widened graph, made `go mod tidy` adopt unrelated
modules (`go.opentelemetry.io/contrib/.../otelhttp` and `stretchr/testify`
via the golang-migrate mysql test chain → dktest → docker client). The
graph **already selected otel v1.44.0** before this increment (an existing
dependency requires it), so pinning all three modules at **v1.44.0** upgrades
nothing else: `go mod tidy` on main stays byte-clean apart from the new
modules. The task's "slightly older but compatible" clause applies.

### Exporter: otlptracehttp, with honest dependency numbers

OTLP/HTTP was chosen over OTLP/gRPC for protocol and ops reasons: collectors
behind plain HTTP are the common in-cluster deployment, the wire surface is
net/http only, and no gRPC dial/channel machinery ever runs.

One correction to the task briefing's prediction: with otel v1.44.0 the
`google.golang.org/grpc` **module does appear in `go.mod` as indirect even
with the HTTP exporter** — `otlptracehttp/internal/otlpconfig` is shared
with the gRPC exporter and references grpc types in its config struct
(`DialOptions []grpc.DialOption`, `GRPCConn *grpc.ClientConn`). No gRPC
client is constructed on the HTTP path. Measured: the HTTP exporter's
transitive build closure is 388 packages vs 387 for `otlptracegrpc` — in
v1.44.0 the tree difference between the exporters is negligible, so HTTP
wins on wire surface, not tree size. The line that was actually held: no
gRPC exporter, no contrib.

Final `go.mod` delta (16 modules; direct = imported by ncgo code):

- direct: `go.opentelemetry.io/otel` v1.44.0, `.../otel/trace` v1.44.0,
  `.../otel/sdk` v1.44.0, `.../otlp/otlptrace/otlptracehttp` v1.44.0
- indirect: `go.opentelemetry.io/otel/exporters/otlp/otlptrace` v1.44.0,
  `.../otel/metric` v1.44.0, `go.opentelemetry.io/auto/sdk` v1.2.1,
  `go.opentelemetry.io/proto/otlp` v1.10.0, `github.com/cenkalti/backoff/v5`
  v5.0.3, `github.com/go-logr/logr` v1.4.3, `github.com/go-logr/stdr` v1.2.2,
  `github.com/grpc-ecosystem/grpc-gateway/v2` v2.29.0 (proto collector
  stubs), `google.golang.org/grpc` v1.82.0 (shared otlpconfig types only),
  `google.golang.org/genproto/googleapis/{api,rpc}` 2026-06-30,
  `google.golang.org/protobuf` v1.36.11

### Middleware: self-written (~80 lines), not contrib otelhttp

The request middleware (`observability.Tracing`) follows the repo's
stdlib-first middleware idiom (`httpx.Middleware` signature), reads exactly
the attributes ncgo wants, and — decisively — integrates with the router's
low-cardinality naming hook (below) and the repo's request-ID mechanism.
otelhttp would add a contrib module for behavior we control better in-house.

### Configuration

- `observability.otel_endpoint` (string, default `""`): empty disables
  tracing **completely** — the app constructs no provider, installs no
  middleware, and passes no provider to the plugin host; the global
  `otel.GetTracerProvider()` default stays the no-op provider, so the
  disabled path is exactly zero overhead. A bare `host:port` is treated as
  **insecure plaintext HTTP** (collectors commonly run naked HTTP
  in-cluster; TLS is terminated upstream); a full `http(s)://` URL is passed
  to `WithEndpointURL`, which honors scheme (http ⇒ insecure) and path.
  Other schemes are rejected at startup.
- `observability.otel_sample_ratio` (float, default **1.0**, validated to
  [0,1]): parent-based head sampling. Default 1.0 because self-hosted
  installs are the primary target — trace volume is bounded by the
  operator's own traffic, and a default that silently drops traces makes
  the feature useless out of the box; high-traffic operators can dial it
  down. `ParentBased` keeps upstream-sampled traces whole.

### Provider lifecycle

`App.New` builds the provider (BatchSpanProcessor over the HTTP exporter;
resource `service.name=ncgo`, `service.version` from `internal/version`,
`service.instance.id` = instance id) only when the endpoint is non-empty.
Construction dials nothing — the HTTP exporter connects lazily on first
flush — so a dead collector never blocks startup. `App.Close` shuts the
provider down with a 5-second flush budget on a fresh context (the incoming
ctx may already be done) and joins any error into the existing
`errors.Join` close chain, after `PluginHost.Close` so plugin spans flush.

### Request spans: naming and cardinality control

The middleware sits directly behind `httpx.Recover` in the base chain, so
panics are recorded as Error spans (marked, ended, re-panicked) before
Recover renders the 500. Per request it extracts the W3C `traceparent`
header (absent or malformed ⇒ new root), starts a server span named by the
bare method, and ends it after the handler returns with
`http.method`/`http.target`/`http.status_code`/`http.client_ip`/`request_id`
attributes. Status semantics: **5xx ⇒ `codes.Error`** with the status in the
description; 4xx and everything else stay Unset (client faults are not
server errors). The status code is captured with a ~30-line
status-recording `ResponseWriter` (implicit-200 aware).

Span names must never contain the raw request path — DAV URLs carry
per-user file paths (`/remote.php/dav/files/alice/...`) and would explode
span-name cardinality. The router therefore tells the middleware which
**registered** route matched: `httpx.Router.ServeHTTP` records the exact
path or registered prefix on the request context
(`httpx.RouteNameFromContext`) and additionally calls
`trace.SpanFromContext(...).SetName(...)` so a span started by an *outer*
tracing layer is renamed too (a no-op span makes that call free). The
base-chain middleware starts its span after dispatch, so it reads the route
from the context and names the span `GET /status.php` /
`GET /remote.php/dav` at start; unmatched paths keep the method-only name.
The raw path appears only on `http.target`. One wrinkle: `RequestID` runs
*downstream* of Tracing in the chain, so the request id never reaches the
middleware's request context — it is read back from the response header the
RequestID middleware sets.

### Plugin host-call spans

At the 4i `wrapHostMetrics` hook (registration time, same reflection shape)
a second, independently switched wrapper `wrapHostTracing` emits
`plugin.host.<function>` spans with `plugin.id`, `ncgo.function`, and
`ncgo.result_code` attributes. Status semantics mirror the metrics
classification: only `ErrCodeInternal` (-1) marks `codes.Error` —
capability denials are expected ABI outcomes, counted separately by the
metrics layer. `abi.go` composes both wrappers
(`wrapHostTracing(wrapHostMetrics(fn))`); each nil-checks its own knob, so
metrics-only, tracing-only, both, and neither all work. `invokeEntry`
derives the host-call context from the request context, so host spans are
children of the request span with no extra plumbing (verified by test with
`tracetest`).

### What is deliberately not here

- ~~**DB query spans**: need a `database`-package-level hook (the DB
  interface is used by every store); a separate increment must evaluate
  where the hook sits and what statement-attribute policy is safe (SQL text
  is high-cardinality and may carry literals).~~ (resolved by ADR-0075)
- Metrics export via OTel: the Prometheus registry (ADR-0055) stays the
  metrics path.
- CLI processes: server-only assembly.

## Consequences

- Tracing disabled costs nothing; enabled costs one server span per request
  plus one span per plugin host call, batched to the collector.
- `internal/httpx` now imports the otel **API** module (for the router
  rename hook); `internal/observability`, `internal/plugins`, and
  `internal/app` import API+SDK. No other packages gain dependencies.
- The zero-dependency posture is otherwise unchanged; further OTel modules
  (contrib, gRPC exporter, metric SDK) need the same explicit approval.

## References

- ADR-0055 (plugin metrics; OTel deferral resolved here)
- ADR-0064 (requesttoken/CSRF chain the middleware joins)
- [otlptracehttp options](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp)
  (`WithEndpointURL` derives TLS from the scheme)
