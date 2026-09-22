# ADR-0056: Phase 4j plugin lifecycle fixes — full install host, on_upgrade, uninstall cleanup

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

A strict review of the Phase 4 plugin stack found three functional gaps:

- **G1 — the CLI install host was an empty shell.** `ncgo-cli plugin
  install` built `plugins.NewHost(ctx, plugins.HostConfig{}, …)`, so every
  hook-only host function that needs a subsystem failed with -12
  (`route_register`/`ocs_register`/`webdav_register_prop` need the registry,
  `db_*` need the DB, `config_*` need appconfig, `job_enqueue` needs the
  runner, `storage_*` need the DAV/system storage). The shipped
  `examples/file-tagger` could not install: its `on_install` runs `db_exec`
  DDL. `pkg/pluginsdk` also had no RouteRegister/OCSRegister binding at all,
  despite the host functions existing since Phase 4c3.
- **G2 — `ncgo_on_upgrade` was dead.** The manifest parsed `on_upgrade`
  (spec §7) but nothing ever invoked it: an upgrade re-ran `on_install`,
  and the old version's `plugin_routes`/`plugin_webdav_props` rows lingered
  because registration is upsert-only.
- **G3 — uninstall leaked, and the jobs runner retried leftovers forever.**
  Uninstall removed routes/props/registry-row/install-dir but not the
  plugin's `plugin.<id>` jobs rows, its `("plugin", <id>.*)` appconfig
  rows, or its `appdata_<instance>/plugins/<id>/` system-storage tree.
  Leftover jobs rows hit the runner's unknown-job path, which
  failed-and-rescheduled on every poll with no attempts cap. A minor
  companion bug: `app.Close` assigned (not joined) the plugin host's close
  error, discarding earlier close errors.

## Decision

1. **G1 — full install-time host in the CLI.** `plugin install`/`uninstall`
   now assemble the same HostConfig subsystem set `app.New` wires: DB, the
   plugins registry, appconfig store, an event bus (hooks may publish), a
   jobs runner, a files DAV (factored into a helper in `deps.go` shared
   with `import-nextcloud files`), and the default storage backend as
   system storage under `appdata_<instance.id>/plugins`, using the server's
   exact instance-id fallback (`instance.id`, else ephemeral `"oc"+hex`).
   Two deliberate exceptions:
   - **Cache stays nil.** The CLI is a management surface; cache is runtime
     state owned by the server process. `cache_*` calls in hooks get -12,
     same as any unconfigured subsystem.
   - **Metrics stay nil.** No listener exists in the CLI.
   The CLI's jobs runner is constructed but **never started** — `Enqueue`
   only inserts rows, which the server picks up on next boot. Separately,
   install/upgrade now register the plugin's `plugin.<id>` job adapter
   before lifecycle hooks run (`startForHook`): previously `job_enqueue`
   from `on_install` failed with `ErrUnknownJob` until the next boot's
   `startOne` registered the adapter. `plugin check` intentionally keeps
   its empty host — it is a compile/ABI smoke test, and running real DDL
   against the configured database from a "check" command would be
   surprising (documented in `examples/file-tagger/README.md`). pluginsdk
   gained thin `RouteRegister`/`OCSRegister` wrappers (+ non-wasm stubs).
   The signature-verification flow is unchanged.

2. **G2 — real upgrade path with clear-then-hook ordering.** When the
   registry already holds the plugin id, `Installer.Install` now — after
   the signature/capability gates and **before** storing the new archive —
   (a) captures the installed version, (b) deletes the old version's
   `plugin_routes` and `plugin_webdav_props` rows, and (c) starts the
   plugin and invokes `on_upgrade` with the from-version string via the
   guest-memory invocation mechanics, **in lifecycle-hook context** (DDL
   and hook-only registrations permitted, exactly like `on_install`).
   Clearing first is what stops upsert-only registrations from lingering
   when the new version no longer declares them. Running the hook before
   storing the archive means a failing upgrade hook leaves the old archive
   installed; its registrations are re-created by the next successful
   install. Plugins **without** an `on_upgrade` entry point fall back to
   `on_install`, so existing route-registering plugins keep working
   unchanged. Fresh installs are untouched (`on_install` only).

3. **G3 — uninstall cleanup plus a runner safety net.** `Installer.Uninstall`
   additionally removes, when the dependencies are wired (the CLI wires all
   of them):
   - the plugin's `plugin.<id>` jobs rows (new `jobs.Store.DeleteByName`),
   - its `("plugin", <id>.*)` appconfig rows (new
     `appconfig.Store.DeleteByPrefix`; LIKE wildcards in the prefix are
     escaped with `ESCAPE '!'` — `!` needs no dialect-specific string
     quoting, unlike `\`),
   - its `<SystemPrefix>/<id>/` system-storage tree (new bounded recursive
     walk over List/Delete in `internal/plugins`: depth ≤ 64, ≤ 100k
     entries, missing root tolerated, bounds are errors rather than silent
     truncation; backends reject deleting non-empty directories, so the
     walk deletes children first).
   As a safety net for leftovers from before this cleanup existed (and any
   future writer of unknown-name rows), the jobs runner now fails an
   unknown-name row at most `maxUnknownJobAttempts` (3) times, then
   completes it with a warn log instead of rescheduling forever. The
   `app.Close` error join was fixed so the plugin host's close error no
   longer discards earlier errors.

## Consequences

- The shipped file-tagger example installs end-to-end via the CLI; any
  plugin whose hooks use db/config/jobs/storage/routes now installs too.
- Upgrading a plugin that stopped declaring a route/prop actually removes
  the stale registration; plugins with `on_upgrade` receive the previous
  version string as spec §7 promises.
- Uninstall leaves no plugin rows in `jobs`/`appconfig` and no
  system-storage tree; pre-existing leftover rows are drained by the
  runner's 3-attempt cap instead of retried forever.
- `plugin install`/`uninstall` now require a configured storage backend
  (same precondition as the server itself).
- No new dependencies; `go mod tidy` is a no-op.

## Deferred

- **Cache-key cleanup on uninstall.** `cache.Cache` (Get/Set/Delete/
  Increment) has no prefix delete or scan, so `plugin:<id>:` keys survive
  uninstall (memory cache dies with the process; Redis keys would need a
  SCAN-based deleter). The interface was deliberately not widened in this
  increment.
- **§13 private-IP egress check** for `http_*` (block private ranges
  unless granted) remains pending.
- **Newly confirmed spec deviations** (recorded in the spec Change Log):
  `runtime.fuel_per_call` is parsed but unenforced (wazero v1 has no fuel
  API; CPU budget is wall-clock timeout only), per-plugin
  `runtime.memory_limit_mb` is validated but not applied (host-global cap
  only), and trap-during-request returns 502 (spec text corrected from
  500).
- Old-version archives under `<install_dir>/<id>/` are kept after upgrade
  (pre-existing behavior; potential rollback story, no GC yet).
