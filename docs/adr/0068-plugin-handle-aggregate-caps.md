# ADR-0068: Phase 4v Plugin Handle Aggregate Caps

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none) — closes the ADR-0060 "cross-instance aggregate
  handle cap" follow-up and revises the spec §8 handle-budget semantics
  from ADR-0039

## Context

ADR-0039 gave plugin handles three budgets — 64 stream, 16 DB rows, 16 HTTP
response — scoped **per instance**: each instantiated module owns a handle
table, and an `add` past the budget fails with `ErrUnavailable`. ADR-0060
then bounded *in-flight* DB statements per plugin (4 concurrent), but
explicitly left the **open-handle** footprint as a follow-up. That footprint
is not bounded per plugin: the pooled instance model runs `pool_size`
instances and per_request runs one per concurrent call, so a plugin's total
open-handle count is `instances × budget` with no aggregate ceiling. Open
rows handles are the sharp edge: each one pins a `database/sql` pool
connection for its whole lifetime, and those idle cursors sit *outside* the
ADR-0060 statement slots (a slot is held only during a `Query`/`Exec`/`Begin`
call). A pooled plugin holding 16 open cursors per instance across a large
pool could drain the shared pool the server itself depends on, denying
database access to other plugins and to core.

ADR-0060's review also surfaced that the `pool_size` manifest validation had
a lower bound (`>= 1`) but no upper bound, so the instance multiplier itself
was unbounded in signed-package state.

## Decision

1. **The §8 budgets become per plugin, shared across instances.** The
   values are unchanged — 64 stream, 16 DB rows, 16 HTTP response — but
   they now bound one plugin's handles aggregated across all of its live
   instances rather than each instance separately. The per-instance handle
   table is retained: handle *ids* must stay instance-scoped (an id handed
   to one instance is meaningless to another), and the table keeps its
   same-value check, which can only fire first for a single-instance fill.
   A single instance can never legitimately hold more than the aggregate,
   so no per-instance-only guest observes a behavior change.

2. **Host-level aggregate accounting.** `Host` holds a
   `(plugin id, handle kind) → count` map under one mutex
   (`internal/plugins/handleagg.go`), initialized at construction.
   `handleTable` gains an optional shared-aggregate pointer plus the plugin
   id; `instanceManager.instantiate` wires both from the manifest and the
   host, so every instance of one plugin draws from the same pool. `add`
   runs the per-table check first and the aggregate acquire second — a
   table-full refusal never touches the aggregate, an aggregate refusal
   consumes nothing, and the insert after a successful acquire cannot fail,
   so neither failure path leaks a slot. `remove` returns one slot;
   `closeAll` returns one slot **per entry**, so the invariant "aggregate
   equals the sum of live tables' counts" holds across every release path —
   clean release, per_request instance close, and trap-destroyed instance
   alike (spec §8: any trap destroys the instance). Map entries are deleted
   at zero, so the map stays bounded by handles actually open and resets on
   restart like the 4n/4o quota state. Host-less tables constructed
   directly by unit tests keep a nil aggregate and degrade to the
   table-only check.

3. **Guest-visible behavior is unchanged.** Aggregate exhaustion produces
   the same `ErrHandleLimit` the per-table check already produced, and the
   ABI call sites keep mapping it to `ErrUnavailable` (-12) — the error
   every guest must already handle when its own table fills. A refusal only
   appears in a situation that previously allowed a cross-instance
   hoarding pattern; sequential and per-instance handle usage is
   unaffected. No new config keys and no new metric families: refusals
   return -12 from inside the wrapped host functions and land in the
   existing §12 `ncgo_plugin_host_calls_total` `result` label.

4. **`pool_size` upper bound.** Manifest validation now requires
   `pool_size` in 1..32 for the pooled model. Every pooled instance is a
   live wasm module with its own linear memory (up to the instance memory
   limit), so the pool multiplies the plugin's worst-case memory and
   handle-adjacent footprint; legitimate pooling needs are single-digit,
   and 32 is a generous ceiling, not a target. Out-of-range manifests fail
   with `ErrManifestInvalid` like the other §4 field validations.

### Division of labor with ADR-0060

The two mechanisms govern different dimensions and both stay in force. The
ADR-0060 statement slots bound *execution in flight* — how many statements
are running at one moment, each slot held only for the call's duration.
The §8 aggregate bounds *retained open handles* — cursors, transactions,
streams, and responses a plugin keeps across host calls within an
instance's lifetime. Neither covers the other: a plugin can hold 16 idle
cursors with zero statements in flight (slots don't see it), and it can fan
out 4 concurrent statements while retaining no handles at all (the
aggregate doesn't see it).

## Alternatives Considered

### Per-plugin connection pools
- Pros: true isolation — one plugin's open cursors cannot starve others.
- Cons: a pool per plugin multiplies connections against small deployments
  (sqlite is single-writer anyway) and adds operator configuration surface.
  ADR-0060 already deferred this; it remains the follow-up hardening option
  if aggregates prove insufficient.

### Raising per-instance budgets instead of sharing them
- Pros: no cross-instance machinery.
- Cons: leaves the multiplication vector open — `pool_size × budget` still
  grows without bound — which is the problem this ADR exists to close.

### Blocking acquire with a channel semaphore
- Pros: waiting guests proceed as slots free up.
- Cons: changes the failure mode from "fast -12 the guest already handles"
  to "guest stalls until the call timeout"; fail-fast matches the 4n/4o
  posture, and the map+mutex is stdlib-only.

### Operator-configurable budgets
- Pros: tunable for exotic plugins.
- Cons: the budgets are ABI contract (spec §8); making them host-local
  config would let the same plugin behave differently across deployments.
  Exhaustion is fail-fast and self-healing, so a knob buys little.

## Consequences

- A plugin's worst-case DB-connection footprint from open rows/tx handles
  is 16 connections regardless of `pool_size` or request concurrency; the
  shared-pool DoS vector is closed.
- Pooled plugins that legitimately streamed more than one budget's worth of
  handles *across instances simultaneously* now see -12 where they
  previously succeeded; the per-instance patterns the ABI documents are
  unaffected, and -12 is a code guests must already handle.
- ADR-0039's follow-up list shrinks to per-plugin pools and read/write
  splitting.
- Spec §8 gains the per-plugin semantic note; the ABI version stays
  `ncgo-abi/1` because the change only tightens a host-side limit within
  already-specified error behavior (§9).

## Verification

- Aggregate unit tests: fill-then-deny boundary, release restores
  availability, zero-count key deletion (no map leak), per-kind isolation,
  per-plugin isolation, and an 8-goroutine acquire/release churn run under
  `-race` that must leave the map exactly empty. Table-level tests pin both
  refusal orderings (table-full and aggregate-full consume no slot) and
  `closeAll` exact return.
- Wasm integration (wasmgen probes + real host, real sqlite/httptest/DAV):
  pooled `pool_size=2` cross-instance refusals for all three kinds (a
  gate-blocked "hold" call keeps 10 rows / 10 responses / 40 streams open
  on one instance while a concurrent "probe" fills the rest on the other —
  the overflow open answers -12); a fill→trap→refill probe proves a
  destroyed instance returns all 16 slots exactly (one leaked slot fails
  the refill's 16th open); a sequential per_request refill proves the
  normal-release path returns everything. Existing singleton/per_request
  handle suites pass unchanged.
- Manifest: `pool_size` 33 rejected, 1 and 32 accepted.
- `go test ./...` green, race clean, golangci-lint 0 issues.
