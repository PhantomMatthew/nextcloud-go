# ADR-0060: Phase 4o Plugin DB Concurrency Quota

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none) — closes the ADR-0039 "per-plugin connection
  pools/quotas" follow-up in its quota form

## Context

ADR-0039 gave the `db_*` host functions SQL parsing, capability globs, and a
**per-instance** handle budget (16 rows/tx handles; an `add` past the budget
fails with `ErrUnavailable`). Two gaps remained. First, under the pooled
instance model a plugin's total open-handle footprint is `pool_size ×` the
per-instance budget with no cross-instance aggregate cap. Second — the gap
this ADR closes — a statement *executing* pins a `database/sql` pool
connection for the whole `Query`/`Exec`/`Begin` call, and nothing bounded how
many statements one plugin could have in flight at once: a plugin calling
from several pooled instances (or overlapping calls in the singleton model is
serialized, but pooled is not) could monopolize the shared pool the server
itself depends on.

Of the other two ADR-0039 follow-ups: query duration in the §12 metrics is
already closed — Phase 4i's `wrapHostMetrics` (ADR-0055) records a latency
histogram labeled by plugin/function for every exported host function,
`db_*` included, so no code change was needed there — and read/write
splitting (routing reads to replicas) is infrastructure-level and stays a
follow-up.

## Decision

1. **Per-plugin in-flight statement quota.** `HostConfig.DBMaxConcurrentPerPlugin`
   (`<= 0` selects the default 4) bounds how many DB statements one plugin
   id has *executing at the same moment*, aggregated across all of the
   plugin's instances. The five connection-consuming entries —
   `db_query`, `db_exec`, `db_tx_query`, `db_tx_exec`, `db_tx_begin`
   (`Begin` pins a pool connection immediately) — acquire a slot after the
   capability/SQL checks pass and before the `Query`/`Exec`/`Begin` call,
   and `defer` the release, so **a slot is held only for the duration of
   the call, never for the lifetime of a handle**. Exhaustion fails the
   call with `ErrCodeQuotaExceeded` (-8) plus a warn-level log naming the
   plugin — the same posture as the 4n HTTP rate limit. Each statement
   already inherits the calling entry point's remaining wall-clock timeout
   (`DefaultCallTimeout` via `invokeEntry`), so a stuck statement cannot
   hold its slot past the call deadline; no per-statement timeout is added.

2. **Semantic boundary.** The quota governs in-flight statements only. Open
   rows/tx handles remain governed by the per-instance handle budget (spec
   §8, three budgets), and a cross-instance aggregate cap on *open handles*
   stays a follow-up. The residual risk is bounded in practice: each
   instance can hold at most 16 rows/tx handles, and instance counts are
   operator-reviewed manifest state (signed packages, `pool_size` validated
   `>= 1`); an explicit upper bound on `pool_size` or an aggregate handle
   cap is the natural shape of that follow-up.

3. **Implementation.** `Host` holds `dbConcMu` + `dbConc map[string]int`
   keyed by plugin id (`internal/plugins/dbquota.go`). Acquire and release
   happen in the same critical section; release deletes the key at zero, so
   the map is bounded by the number of plugins with statements actually in
   flight and resets on process restart. The plugin id comes from
   `callFromCtx(ctx)` with a nil guard (empty id when no call context is
   present — defensive; exported entries always carry one).

4. **Configuration.** `plugin.db_max_concurrent_per_plugin` (default 4)
   joins `internal/config`'s Plugin section with non-negative validation;
   `internal/app` and the `ncgo-cli` install host both pass it through —
   install/upgrade hooks run DDL/DML through `db_*` and must not bypass the
   quota. Zero explicitly set means the host default; there is no
   "unlimited" setting, matching the default-deny posture.

5. **Metrics.** No new families. Refusals return -8 from inside the wrapped
   host function, so they land in the existing
   `ncgo_plugin_host_calls_total` `result` label (`ClassifyResult` maps -8
   to `internal`) and the call's duration in the §12 histogram; the
   `capability_denials` family stays scoped to -3 because a quota refusal is
   not a capability denial. The warn log carries the security signal.

### Why the default is 4

The quota bounds *concurrency*, not throughput: a plugin issuing statements
sequentially never notices it. Intra-request statement fan-out is rare and
small in the plugin shapes we ship (routes, jobs, event handlers), and
sqlite deployments serialize writes regardless. 4 lets one plugin overlap a
modest fan-out across pooled instances while keeping its worst-case share
of the shared `database/sql` pool small; because a slot outlives only one
statement and the call timeout caps statement duration, even full
exhaustion self-heals within seconds. Larger defaults would only raise
pool pressure without buying correctness.

## Alternatives Considered

### Per-plugin connection pools
- Pros: true isolation — one plugin's open handles cannot starve others.
- Cons: a pool per plugin multiplies connections against small deployments
  (sqlite is single-writer anyway) and adds operator configuration surface.
  Remains the follow-up hardening option ADR-0039 named.

### Counting open handles instead of in-flight statements
- Pros: would also cap idle cursors pinning connections.
- Cons: duplicates the existing per-instance handle budget's job across
  instances, and breaks the legitimate pattern of streaming a large result
  set through a long-lived cursor. The in-flight quota targets the
  unbounded dimension (concurrent execution) without touching governed
  ones.

### Channel semaphore per plugin
- Pros: blocking acquire with context cancellation for free.
- Cons: blocking changes the failure mode from "fast -8 the guest can
  handle" to "guest stalls until timeout"; fail-fast matches the 4n
  rate-limit posture, and the map+mutex is ~20 lines of stdlib (the 4i/4n
  zero-new-dependency precedent).

## Consequences

- ADR-0039's follow-up list is now: cross-instance aggregate handle cap /
  per-plugin pools, and read/write splitting.
- A plugin that legitimately needs more than 4 concurrent statements gets
  an operator knob (`plugin.db_max_concurrent_per_plugin`), not a code
  change.
- Quota state is process-local and resets on restart — acceptable, as with
  the 4n buckets: in-flight statements do not survive a restart anyway.
- The wasm-plugin-abi Change Log records the quota semantics; the config
  key appears in the Phase 0 blueprint YAML block.

## Verification

- Quota helper unit tests: N=2 fill-then-deny boundary, release restores
  availability, per-plugin-id isolation, zero-count key deletion (no map
  leak), HostConfig zero-value defaulting.
- Wasm integration (real sqlite + wasmgen probes): with
  `DBMaxConcurrentPerPlugin: 1` and the only slot held at the host layer,
  the probe's `db_query` returns -8 and the warn log names the plugin; with
  quota 1 and no contention the full DBModule flow (install DDL/DML, probe
  SELECT, EOF, close) succeeds, proving acquire/release sequencing.
  Concurrency is orchestrated by holding a slot on the host side rather
  than racing two guests: host functions execute synchronously, so a
  two-guest race would be a flaky test, not a stronger one.
- Config: defaults snapshot, full-file parse, negative-value validation.
- `go test ./...` green, race clean, golangci-lint 0 issues.
