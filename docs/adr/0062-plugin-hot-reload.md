# ADR-0062: Phase 4q Plugin Hot Reload (Runtime Reconciler)

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none) — closes the ADR-0041 follow-up "routes mount at
  boot; runtime refresh is future work"

## Context

Every plugin lifecycle operation — `ncgo-cli plugin install`, `enable`,
`disable`, `uninstall`, and upgrade-by-reinstall — writes the registry
(and, since Phase 4j's clear-then-hook, the route/prop rows) from the CLI
process. The server read that state exactly once: `plugins.StartEnabled`
at boot plus `MountRoutes` when routes were wired, so a running server
never noticed the change and Phase 4k had to document "restart required"
in the README, the example READMEs, and the CLI help. Three structural
gaps blocked a runtime refresh:

1. `httpx.Router` had no locking and no way to remove a route —
   registration was boot-time-only by construction.
2. `jobs.SQLRunner` had no `Unregister`, so disabling then re-enabling a
   plugin left the runner holding a stale job adapter pointing at a dead
   `*Plugin` (and re-registration would have hit `ErrDuplicateJob`).
3. Unmounting cannot ask the registry "which routes did this plugin
   have?": by the time an uninstall is observed, the CLI has already
   deleted both the route rows and the registry row.

## Decision

1. **Polling reconciler.** A new `plugins.Reconciler`
   (`internal/plugins/reconcile.go`) owns the running plugin set:
   `Sync(ctx)` is one idempotent pass, `Run(ctx, interval)` is the
   blocking poll loop the app runs in a goroutine, and `Close(ctx)` stops
   every tracked plugin for `App.Close`. Each pass diffs
   `Registry.ListEnabled` against its own tracked state
   `map[id]{plugin, version, routes}`:
   - **enabled, not running** → `startOne` (unchanged: read archive,
     `Load`, `Start` with warm + ABI check, attach for events, register
     the job adapter), then mount its persisted route rows;
   - **running, no longer enabled** (disable, or uninstall — the row is
     simply gone) → stop;
   - **running, registry version changed** → stop then start (upgrade:
     the CLI has already stored the new archive and rewritten the route
     rows, so the fresh start reads both).
   Per-plugin failures stay isolated the `startOne` way — logged, skipped,
   retried on the next pass. A registry read failure aborts the pass and
   keeps the currently running set until the next tick. The boot path is
   the same mechanism: `mountRoutes` constructs the reconciler and runs
   the first `Sync`, replacing the former `StartEnabled` + `MountRoutes`
   pair; `refresh_interval = 0` then never starts `Run`, preserving the
   pre-4q startup-mount semantics exactly.

2. **Router concurrency and removal.** `httpx.Router` gained a
   `sync.RWMutex` and `Remove(method, path)`. Registration/removal take
   the write lock; `ServeHTTP` resolves the handler — including the 405
   `Allow` computation — under a single read lock and invokes it *after*
   unlocking (Go's `RWMutex` forbids recursive read locking, and a slow
   plugin handler must not block registration). `Remove` serves exact
   routes only (the only kind plugins mount), silently ignoring unknown
   pairs and prefix routes, and drops the method from the path's `Allow`
   bookkeeping.

3. **Stop ordering: unmount routes → unregister job adapter →
   `Plugin.Close`.** Routes come off first so no *new* request dispatches
   into a plugin that is about to close; removal cannot affect requests
   already dispatched — `ServeHTTP` handed them the handler before
   unlocking, so they run to completion against the still-open plugin.
   This is strictly gentler than the old restart semantics (a restart
   dropped in-flight requests outright). `Plugin.Close` detaches event
   delivery as before; no other Host mechanism (metrics, WebDAV props —
   looked up per request since Phase 4c8, ADR-0046) needed reconciliation.

4. **Jobs: `Runner.Unregister(name)`.** The `jobs.Runner` interface and
   `SQLRunner` gain `Unregister(name string)`; unknown names are a silent
   no-op (the reconciler unregisters unconditionally — a plugin without
   `jobs.register` or `on_job` never registered). Unregistering frees the
   `plugin.<id>` name so the next start's `Register` no longer trips
   `ErrDuplicateJob` and, more importantly, no stale adapter keeps
   dispatching to a closed `*Plugin`. Rows a plugin queued *while
   disabled* are not deleted: with no adapter registered they retry as
   unknown-name and are dropped after the runner's existing three-strikes
   cap (`maxUnknownJobAttempts`). We accept that semantic — a disabled
   plugin's queued work expiring is indistinguishable from the pre-4j
   uninstall-leftover case the cap was built for.

5. **Route removal works from mount-time keys, not the registry.** The
   reconciler records every route key it mounts (`method` + full path;
   an OCS record mounts under both `/ocs/v1.php` and `/ocs/v2.php`, so it
   yields two keys). Uninstall deletes the route rows before the server
   ever sees the change, so by stop time the DB cannot answer "what was
   mounted" — the tracked list can.

6. **Configuration.** `plugin.refresh_interval` (duration, default 10s)
   joins the `PluginConfig` section; negatives are rejected, `0` disables
   the poll (restart-only, the pre-4q behavior). The first `Sync` runs
   regardless, so boot mounts are unaffected by the setting.

## Alternatives Considered

### Admin endpoint (`POST /ocs/v2.php/apps/ncgo/plugins/refresh`)
- Pros: instant application, no poll latency, no idle wakeups.
- Cons: a new authenticated management surface that the CLI (a separate,
  offline-capable process) would have to call — breaking the clean
  "CLI writes DB, server reads DB" split; needs authn/authz decisions and
  CSRF treatment; still needs a boot path. The poll interval (10s
  default) makes the latency argument weak for an operator-driven action.

### SIGHUP (or other signal) triggered reload
- Pros: conventional for daemons, no endpoint surface.
- Cons: signals carry no payload and no result — the CLI cannot tell
  whether the reload happened or failed; Windows support is poor; still
  needs the same reconciler machinery underneath. Operators get the same
  effect today by setting a short interval or restarting.

### Watch the database (SQLite WAL hooks / Postgres LISTEN/NOTIFY)
- Pros: event-driven, no poll delay.
- Cons: dialect-specific plumbing for a cross-dialect codebase, and
  notification loss still forces a polling fallback. Polling the
  registry's `ListEnabled` is dialect-neutral and the failure mode
  (outage → keep current set) is trivially correct.

## Consequences

- Enable/disable/install/upgrade/uninstall take effect within one
  `plugin.refresh_interval` on a running server; a restart still works
  and is required when the interval is 0. The 4k "restart required"
  texts were rewritten accordingly.
- Detection keys on the registry row's `version` (plus `enabled`): a
  same-version reinstall is not detected — bump the version or restart.
- In-flight plugin requests survive an unmount (they already hold the
  handler); a request arriving between route removal and `Plugin.Close`
  can still enter the plugin — the module is open and the call-timeout
  bounds it. No request ever dispatches into a *closed* module.
- Reconciler state is in-memory: a crash/restart simply rebuilds it from
  the registry at the boot-time `Sync`.
- ~~Follow-up~~ (**resolved by ADR-0063**): with hot reload, ADR-0058's
  "memory-cache uninstall residue
  dies with the restart uninstalls require" argument no longer holds —
  uninstall + reinstall within one process lifetime can read stale
  `plugin:<id>:` keys from a memory L1. ~~A reconciler-side cache purge
  when a stopped plugin's registry row is gone (uninstall, not disable)
  is the documented fix~~ The reconciler now purges exactly then; Redis
  deployments are already covered by the CLI.

## Verification

- Router: existing suite green; new `Remove` tests (405 `Allow`
  bookkeeping, no-op cases, prefix immunity) and a concurrent
  register/Remove/ServeHTTP churn test under `-race`.
- Jobs: `Unregister` frees the name for re-registration and unknown
  names are no-ops; the `errJobRunner` stub gained the method.
- Reconciler integration (wasmgen route probes over real sqlite):
  enable → route 200, idempotent re-`Sync`, disable → 404 (plain + both
  OCS mounts), re-enable → 200; upgrade → new archive serves the v2
  body; uninstall (route rows deleted first) → 404 via tracked keys;
  broken archive isolated (good plugin unaffected, Sync nil, retried and
  mounted once the archive lands); job adapter unregistered on disable
  (`ErrUnknownJob`) and re-registered without `ErrDuplicateJob`;
  registry outage keeps the running set and returns the error; `Close`
  unmounts everything and makes `Sync` a no-op; `Run` hot-applies a
  disable within one interval and exits on ctx cancel; interval 0
  returns immediately.
- Config: defaults snapshot (10s), full-file parse, negative rejection.
- `go test ./...` green, `-race` clean, golangci-lint 0 issues.
