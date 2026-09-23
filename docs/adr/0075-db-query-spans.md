# ADR-0075: Phase 5c database query spans — decorator at the DB interface

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0072 (resolves its DB-spans follow-up)

## Context

ADR-0072 shipped request and plugin host-call spans and explicitly deferred
DB query spans: they need "a `database`-package-level hook (the DB interface
is used by every store)" and a statement-attribute policy that is safe
against cardinality and literal leakage. Every SQL call in ncgo flows
through the small `database.DB`/`Tx` interfaces (`Query`, `QueryRow`,
`Exec`, `Begin`, `Commit`, `Rollback`, `Ping`) — users, sessions, shares,
DAV metadata, jobs, appconfig, the plugin registry, and the plugin host's
guarded DB access all hang off the one pool `App.New` opens. That makes
the interface a natural single choke point for tracing.

Two facts from the existing code shape the design:

- `Rebind` runs **inside** `sqlDB`, below the interface: a decorator at the
  interface always sees the squirrel-built SQL with `?` placeholders, which
  is static per call site and carries no literal values. The statement text
  is therefore safe to attach as an attribute, but never usable as a span
  name (a name must be low-cardinality by construction, not by caller
  discipline).
- The result types are interfaces too (`Rows`, `Row`, `Result`), so the
  decorator can wrap results and end spans at the exact moment a result is
  consumed — no callbacks, no goroutines.

## Decision

1. **Decorator at the DB interface, not driver-level otelsql, not per-store
   wrapping.** `database.WithTracing(inner DB, tp trace.TracerProvider) DB`
   returns a `DB` that wraps every call in a `SpanKindClient` span; a nil
   provider returns `inner` unchanged, keeping the ADR-0072 posture that
   disabled tracing is exactly zero overhead. Driver-level instrumentation
   (`otelsql` from contrib) was rejected: it adds a new contrib dependency —
   the ADR-0072 approval covered exactly three modules, no contrib — and it
   would sit *below* `Rebind`, seeing post-rewrite `$N`/`?` text while the
   pre-Rebind statement is the stable, call-site-static form; wrapping the
   stdlib driver would also bypass our own pool/open logic. Per-store
   wrapping was rejected: ~20 stores would each need a wrapping call (one
   missed store is a silent blind spot), while one wrap at the source in
   `App.New` covers every present and future store — the same
   stdlib-first/single-choke-point argument as ADR-0055's middleware stance.

2. **One wrap at the source, before any store is built.** `App.New`
   constructs the TracerProvider immediately after `database.Open` and
   migrations succeed (the `instanceID` assignment moved ahead of it, since
   the resource carries `service.instance.id`), then wraps:
   `if a.tracing != nil { a.DB = database.WithTracing(db, a.tracing) }`.
   Every store built afterwards — users, sessions, shares, DAV meta, jobs,
   appconfig, plugin registry, `HostConfig.DB` — shares the traced DB, so
   the single wrap covers all DB access including the plugin host's. The
   "empty endpoint = no provider at all" semantics and comment are
   unchanged; the provider lifecycle (5s flush budget in `Close`) is
   unchanged. Migrations keep the unwrapped `*sql.DB` via `database.Unwrap`
   before the wrap (schema management, not the query path).

3. **Span shape: low-cardinality by construction.** The span name is the
   uppercase first SQL keyword (`SELECT`, `INSERT`, `UPDATE`, `DELETE`,
   `REPLACE`, `BEGIN`, `COMMIT`, `ROLLBACK`, `PING`, `CREATE`, ...); an
   empty statement names the span `SQL`. The statement text is never part
   of the name. Attributes: `db.system` (`sqlite`/`mysql`/`postgresql`
   from `Dialect()`), `db.operation` (the lowercase keyword), and
   `db.statement` (the SQL as received — pre-Rebind `?`-placeholder form,
   truncated at 4096 bytes with a `…` suffix, backing off to a rune
   boundary). `Exec` success adds `db.rows_affected` (int64, only when
   `RowsAffected()` itself succeeds); `Query` adds `db.rows_returned`
   (int64) at span end, counting `Next()==true` iterations. Cardinality
   analysis: names are bounded by the SQL verb set (~a dozen values);
   `db.statement` values are bounded by the number of static call sites in
   the tree (squirrel builders produce identical text per call site), and
   the 4096-byte truncation plus the collector's own attribute limits cap
   the per-span cost. Placeholders mean literals never appear, so the
   attribute is not a PII channel for the query path.

4. **Lifecycle: every span ends exactly once.** `sync.Once` guards each
   end. `Query`'s span starts at the call and ends on `Rows.Close()` or on
   `Next()` returning false, whichever comes first; at end, a non-nil
   `Rows.Err()` marks the span Error. A start error (Query returns err)
   ends the span immediately with Error. `QueryRow`'s span ends at the
   first `Scan` — the codebase invariant is that QueryRow results are
   always Scanned (stores never discard a Row); a never-Scanned Row would
   leak an unended span, which shows up as a *missing* span in the backend
   (the recorder/exporter simply never sees it end), never as wrong data.
   `Exec`, `Ping`, `Commit`, `Rollback` end at return. `Begin` ends at
   return with name `BEGIN`, and the returned `Tx` wrapper carries the
   Begin context so in-transaction statement spans (and `COMMIT`/
   `ROLLBACK`) parent to the BEGIN span and share its trace; the caller's
   ctx still drives the inner calls (cancellation). `Close` gets no span:
   pool teardown is local resource management, not a database call.
   `Dialect()` passes through.

5. **Errors: Error status, except not-found.** Failures record
   `span.RecordError(err)` plus `SetStatus(codes.Error, err.Error())`.
   The one exception: `QueryRow`'s `Scan` returning `sql.ErrNoRows`
   (re-exported as `database.ErrNoRows` — the same value, so one
   `errors.Is` covers both spellings) is a normal not-found lookup, not a
   failure; the span ends without Error status and without a recorded
   exception, mirroring ADR-0072's "4xx is not a server error" stance.

6. **No new config.** `observability.otel_endpoint` and
   `observability.otel_sample_ratio` govern DB spans too — per-query span
   volume on a busy instance is precisely the reason a ratio below 1.0
   exists. The CLI (`ncgo-cli openDB`) is deliberately **not** traced: it
   is a short-lived process with no provider lifecycle (ADR-0072 scopes
   tracing to the server assembly), and each command performs a handful of
   queries whose spans would race process exit.

## Alternatives Considered

### Driver-level instrumentation (contrib otelsql)
- Pros: spans for hand-written `database/sql` users too; maintained
  semantic conventions.
- Cons: new contrib dependency beyond the ADR-0072 approval; sits below
  `Rebind` (post-rewrite text) and below our pool; cannot see the
  interface-level result lifecycle (Rows/Row are our interfaces), so
  row-count and end-once semantics would need wrappers anyway. Rejected.

### Per-store wrapping
- Pros: stores could opt out individually.
- Cons: ~20 call sites to keep in sync forever; a forgotten store is a
  silent blind spot; zero benefit since no store should be untraced.
  Rejected — one wrap at the source covers all stores by construction.

### Span per row / per Scan
- Pros: finer-grained timing.
- Cons: span volume explodes with result-set size; the useful signal is
  per-statement. Rejected; `db.rows_returned` carries the volume signal.

## Consequences

- Tracing disabled costs nothing (nil provider returns the inner DB
  byte-identical); enabled costs one client span per database call,
  batched with the request spans they now parent under (request → BEGIN →
  statement → COMMIT trees for transactions).
- `internal/database` imports the otel **API** module only
  (`trace`/`attribute`/`codes`); `go.mod` gains nothing — all three are
  already direct dependencies. The SDK enters the package only in tests
  (`tracetest`).
- `App.New` constructs the provider earlier than ADR-0072 described
  (before store wiring, not after), which also makes bootstrap-time DB
  work (`EnsureBootstrapAdmin`, migrations excluded) traced.
- The never-Scanned-Row caveat (Decision 4) is the only leak shape; it is
  a missing span, never wrong data, and no current caller exhibits it.

## Verification

- `internal/database/tracing_test.go` (sqlite in-memory + `tracetest`
  SpanRecorder): Ping/Exec/Query/QueryRow/Begin/Commit/Rollback each emit
  exactly one `SpanKindClient` span with the right name, `db.system`,
  `db.operation`, and `db.statement`; Query spans end exactly once via
  Close *and* via iteration exhaustion (double Close and late Next are
  no-ops); QueryRow ends at the first Scan (a repeated Scan is a no-op);
  the tx tree parents INSERT/SELECT/COMMIT/ROLLBACK to BEGIN; Exec with
  bad SQL and a Query start error produce Error status plus a recorded
  exception; QueryRow over zero rows ends Unset with no events; a
  >4096-byte statement is truncated with `…`; spans started inside a
  parent share its trace ID; nil provider returns the identical inner DB;
  `spanName`/`truncateStatement` unit tables (empty → `SQL`, case folding,
  leading whitespace, rune-boundary backoff).
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` a no-op,
  `go test -race ./internal/database/ ./internal/app/` green, database
  coverage maintained.

## References

- ADR-0072 (OTel spans; DB-spans follow-up resolved here)
- ADR-0055 (stdlib-first posture the decorator choice extends)
- [OTel database semantic conventions](https://opentelemetry.io/docs/specs/semconv/database/)
  (attribute naming followed loosely; pinned to the three modules ADR-0072
  approved)
