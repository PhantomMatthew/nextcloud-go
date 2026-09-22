# ADR-0045: Phase 4c7 Plugin Jobs (job_enqueue + on_job delivery)

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Spec §6.3 defines `ncgo.job_enqueue(name, payload, run_at_unix_ms)` gated by
the `jobs.register` capability with auto-namespaced job names, and §7 defines
the guest's `ncgo_on_job(name, payload) -> i32` entry point. Phase 4a
registered `job_enqueue` as a stub returning `ErrUnsupported` after a coarse
capability check. The host already has a durable SQL jobs runner
(`internal/jobs`, Phase 2h) with at-least-once semantics and infinite retry
on failure — which a naive plugin wiring would turn into a poison-message
loop. This increment lands the real implementation with explicit
drop-vs-retry rules.

## Decision

1. **One adapter job per plugin.** Every plugin job row is stored under the
   single runner-visible name `plugin.<plugin_id>` (the spec's
   auto-namespacing rule). The actual plugin-local name travels inside the
   payload: a MessagePack envelope `{name: string, payload: []byte}`. The
   adapter (`pluginJob` in `internal/plugins/jobs.go`) decodes the envelope
   and delivers name and payload **verbatim** to the guest's `on_job` entry
   point — the namespaced runner name never reaches the guest. One adapter
   per plugin keeps `jobs.Runner` registration trivial and makes enqueue
   validation independent of which local names a guest has used.

2. **Drop vs retry.** `pluginJob.Run` returns nil (the runner completes and
   discards the row) for work that can never succeed: the plugin is no
   longer attached (disabled/uninstalled since enqueue — checked against the
   event dispatcher's attach set via a new `Host.isAttached`), the envelope
   fails MessagePack decoding (poison message, logged at warn), or the
   manifest has no `on_job` entry point. A guest-side failure — non-zero i32
   or trap — returns the error so the runner retries at `now + poll` with
   the attempt recorded. This avoids the runner's infinite retry for
   permanently undeliverable rows while preserving at-least-once delivery
   for transient plugin failures.

3. **Shared delivery mechanics.** The alloc/write/call/free guest-memory
   dance from `deliverEvent` is extracted into
   `Plugin.invokeEntry(ctx, entry, args ...[]byte) error`, which returns
   traps (instance destroyed) and non-zero i32 results (`*PluginError`).
   `deliverEvent` keeps its log-and-continue behavior in the dispatcher;
   the job adapter propagates the same errors to the runner instead.

4. **Enqueue validation and namespacing.** `job_enqueue` validates the
   plugin-local name (non-empty, ≤ 128 bytes, `[A-Za-z0-9_.-]+`, else -2),
   checks `jobs.register` (-3), requires a configured runner (-12), and
   rejects enqueue when the plugin declares no `on_job` entry point (-2):
   such a row could never be delivered, so failing fast beats queuing dead
   work. `run_at_unix_ms <= 0` or in the past means now; more than ten
   years out is rejected (-2). The payload is bounded by the standard 1 MiB
   `maxPayloadArg` cap. Runner errors are logged and surface as -1.

5. **Registration at start.** `startOne` (the boot path) registers the
   adapter right after the event attach when a runner is configured, the
   `jobs.register` capability is granted, and `on_job` is set.
   `jobs.ErrDuplicateJob` (plugin restarted) is tolerated; other
   registration errors are logged and never fail boot — consistent with the
   per-plugin failure isolation of `StartEnabled`. `HostConfig` gains
   `Jobs jobs.Runner`; app.go passes the same `jr` the core jobs use (the
   runner is constructed and started before the plugin host, so no reorder
   was needed).

6. **No user context in job runs.** Jobs execute from the runner's
   background context: there is no request and no `CallContext.UserID`, so
   user-scope storage is unavailable inside `on_job` (-12). Plugins needing
   a user identity must carry it in the job payload. (Events solve this by
   adopting `events.Event.UserID`; job rows have no such field yet.)

## Alternatives Considered

### One runner job name per plugin-local name
- Pros: per-name scheduling visibility in the jobs table.
- Cons: the runner requires pre-registered names, so every new local name
  would need a dynamic registration path from `job_enqueue` (racy with
  `ErrDuplicateJob`, unbounded name set from guest input); the envelope
  approach keeps the registered set exactly one per plugin.

### Deleting queued rows on plugin detach
- Pros: no dead rows ever linger.
- Cons: requires a plugin-id-indexed delete on the jobs table and a detach
  hook into the store; the drop-on-delivery rule achieves the same effect
  lazily, one poll later, with zero schema or store changes.

### Failing boot when adapter registration fails
- Pros: surfaces misconfiguration loudly.
- Cons: a plugin must never take the server down (`StartEnabled` contract);
  a logged error plus undeliverable (dropped) jobs is the same failure
  posture as a plugin that fails to start at all.

## Consequences

- Guest-visible contract: `pluginsdk.JobEnqueue(name, payload, runAtUnixMS)`
  and `pluginsdk.JobArgs(...)` (mirroring `EventArgs`), plus the
  `ncgo_on_job` export. Job state is inspectable in the jobs table under
  `plugin.<id>` with attempts/last_error as usual.
- Detached plugins' queued rows survive in the table until claimed, then
  complete immediately — visible as completed rows, never executed.
- Follow-ups: an envelope `user` field (or a jobs-table user column) so
  `on_job` can act in a user scope; scheduled/recurring plugin jobs (the
  runner's periodic mechanism is currently hardcoded to core jobs);
  uninstall-time cleanup of queued `plugin.<id>` rows.

## Verification

- `job_enqueue` through wasm guests: no capability → -3; nil runner → -12;
  empty/invalid/overlong name → -2; run_at more than ten years out → -2;
  no `on_job` entry point → -2; runner failure → -1 (logged); success → 0
  with the physical row inspected as `plugin.<id>` carrying the
  `{name, payload}` envelope.
- End-to-end with a real SQLRunner on sqlite (20 ms poll): guest
  `do_enqueue` → adapter delivery → guest logs `job ping hello-job`,
  proving the plugin-local name passes through verbatim.
- Non-zero guest code and guest trap both surface as runner retries
  (attempts ≥ 1, `last_error` recorded, row incomplete); after `Close`
  detaches the plugin the next retry drops the row (completed).
- Malformed envelope → dropped (completed) with a warn log; detached plugin
  → `Run` nil without touching the guest; duplicate adapter registration
  tolerated; registration skipped (nil runner / no capability / no entry
  point) leaves the name unregistered.
- `go test ./...` green, race clean, golangci-lint 0 issues,
  `GOOS=wasip1 GOARCH=wasm go build ./pkg/pluginsdk/` clean.
