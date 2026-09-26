# ADR-0095: Phase 5v per-plugin memory introspection and retrospective memory_limit_mb enforcement

- **Status**: Accepted
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0055 (resolves the memory high-water carve-out), ADR-0091
  (resolves its out-of-scope note)

## Context

Two long-deferred items rested on a false premise. ADR-0055 deferred the
per-plugin dashboard's memory high-water mark as "blocked on wazero memory
introspection"; ADR-0091 carried the carve-out verbatim, and the ABI spec
recorded `runtime.memory_limit_mb` as validated (≤ 256, manifest.go) but not
applied. The premise was wrong: wazero's `api.Module.Memory().Size()` has
always reported a module's live linear-memory size in bytes — no
introspection support was ever missing for **measurement**. What wazero
genuinely lacks is mid-call per-module hooks (grow notifications, per-module
memory limits), which constrains how enforcement can react, not whether the
size can be read.

Meanwhile an unapplied `memory_limit_mb` means a guest that balloons its
linear memory keeps its instances — pooled and singleton models reuse them
indefinitely — and only the host-global `WithMemoryLimitPages` clamp
(`DefaultMemoryLimitMB`, max 256 MiB) stops a grow, mid-call, with a trap.

## Decision

1. **Gauge family kind in the registry** (`internal/observability`):
   `RegisterGauge` / `SetGauge` / `SetGaugeMax` join counters and histograms.
   `SetGaugeMax` — set iff greater, atomic under the registry lock — is the
   high-water primitive; concurrent callers can never lower the mark. Render
   emits `# TYPE <name> gauge` with counter-identical label rendering, and
   `GaugeSeries(name)` mirrors the `CounterSeries` read model. Two new
   pre-registered families:
   `ncgo_plugin_memory_high_water_bytes{plugin}` (gauge) and
   `ncgo_plugin_memory_limit_exceeded_total{plugin}` (counter).

2. **Measure at release and close** (`internal/plugins/instance.go`):
   `instanceManager.observeMemory` reads `mod.Memory().Size()`, set-maxes the
   high-water gauge, and — when the manifest sets `memory_limit_mb` and the
   size exceeds it — increments the exceeded counter, logs a Warn
   (`plugin.id`, `memory.bytes`, `memory_limit_mb`), and reports the breach.
   It runs at every release decision and every `instance.close` call site:
   pooled release, singleton release, per_request close, trap destroy, and
   shutdown `closeAll` — so traps and shutdowns record the high-water too. A
   nil registry returns immediately (metrics off = zero overhead).

3. **Retrospective limit enforcement**: an over-limit instance is destroyed
   exactly like a trapped one — pooled destroys and replenishes with a fresh
   instance, singleton drops its single, per_request closes as it always
   does. The completed call's result is **not** failed; the budget is
   enforced *between* calls. The in-call hard ceiling stays the
   runtime-global `WithMemoryLimitPages` clamp.

4. **Console**: the per-plugin metrics panel (ADR-0091) gains memory
   high-water (bytes → MiB with one decimal via the panel's existing byte
   formatter) and the limit-exceeded count per row.

## Alternatives Considered

### Per-plugin wazero runtimes for mid-call enforcement

- Pros: a true per-plugin in-call ceiling — a breaching grow traps that
  plugin's call immediately instead of being noted afterwards.
- Cons: one runtime per plugin means one compilation cache per plugin
  (compile cost and memory multiplied by plugin count), and wazero still
  offers no per-module grow hook, so the ceiling would remain a trap
  boundary, not a budget. Rejected as disproportionate: the global clamp
  already bounds any single call, and retrospective destroy bounds reuse.

## Consequences

- `runtime.memory_limit_mb` gains real (between-calls) teeth: breaching
  instances are destroyed, counted in
  `ncgo_plugin_memory_limit_exceeded_total{plugin}`, and logged at Warn.
- The ADR-0055 dashboard deferral is fully closed — memory high-water was
  its last outstanding item; ADR-0091's out-of-scope note is struck.
- Introspection costs one `Size()` read plus a gauge update per
  release/close when metrics are enabled; a nil registry is byte-identical
  to before. Per-plugin runtimes stay rejected (recorded above).
- A guest can still burst past its per-plugin limit inside one call — the
  host-global 256 MiB clamp remains the only in-call ceiling, by design.

## Verification

- `internal/observability`: gauge register/set/set-max/render/read
  round-trip, `SetGaugeMax` monotonicity (lower writes ignored, concurrent
  max under `-race`), unknown-name and wrong-kind panics consistent with
  counters, zero-series render covers both new families.
- `internal/plugins`: wasmgen `MemGrowModule` grows linear memory in
  `ncgo_on_install`; baseline gauge ≥ grown size with no limit and no
  breach counted; pooled 1 MiB limit vs 2 MiB growth → Install succeeds, the
  exceeded counter is 1, the Warn line carries plugin/size/limit attrs, and
  a subsequent entry call succeeds on the replenished instance; singleton
  breach destroys the single (next call works); nil registry → no panic, no
  log spam, no enforcement.
- `internal/console`: the plugins payload test pins the new fields —
  gauge present, gauge absent (zero), gauge-only plugin row, and the
  metrics-disabled 503 unchanged.
- Full suite + `go test -race` green; `golangci-lint run ./...` 0 issues;
  `go mod tidy` no diff (no new dependency).
