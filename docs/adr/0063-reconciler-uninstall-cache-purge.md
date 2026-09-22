# ADR-0063: Reconciler cache purge on uninstall detection

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0058 (memory-cache residue argument), ADR-0062 (recorded
  this gap as a follow-up)

## Context

ADR-0058 added `Cache.DeleteByPrefix` and wired it into the CLI's uninstall
path, but only for Redis-backed deployments. The argument for skipping
memory deployments was: the CLI process's memory cache is an empty shell,
the server's L1 dies with the process, and uninstalls take effect on restart
anyway — so residue never survives to a reinstall.

ADR-0062 (hot reload) broke that premise: uninstall and reinstall now take
effect on a *running* server within one refresh interval. In a memory-only
deployment a same-process uninstall → reinstall would let the new plugin
generation read the previous one's `plugin:<id>:` keys. The CLI-side purge
cannot help; it runs in a different process and (for memory deployments) has
no cache handle at all.

## Decision

The reconciler closes the hole where it can actually see both events. When
`Sync` stops a plugin, it now distinguishes the stop cause by re-reading the
registry:

- **Row gone (`ErrPluginNotFound`) = uninstall** → purge
  `plugin:<id>:` via the host's cache (`DeleteByPrefix`); failures are
  logged and never interrupt the stop (the keys are inert data).
- **Row present = disable or upgrade** → keep the keys. A disabled plugin's
  state survives for re-enable, and an upgrade's state is managed by the
  plugin's own `on_upgrade` hook — matching ADR-0056's jobs/appconfig/
  storage semantics, which also purge only on uninstall.
- **Registry read failure** → skip the purge conservatively and warn; the
  next uninstall reconciliation retries.

For Redis deployments this duplicates the CLI's purge (ADR-0058) with a
harmless 0-key result; for memory deployments it is the only purge path.

## Consequences

- `Reconciler.stopOne` calls `purgeCacheIfUninstalled`, which needs the
  host's `Cache` (nil → skip, same as every other host dependency) and the
  registry.
- The distinction costs one extra indexed `Get` per stop — negligible at
  reconcile cadence.
- Cache-key lifecycle is now coherent across the four state transitions:
  disable keeps, upgrade keeps (plugin-managed), uninstall purges
  (CLI for Redis, reconciler for memory), server shutdown drops (memory).
