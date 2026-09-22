# ADR-0044: Phase 4c6 Plugin Config Host Functions

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Spec §6.3 defines the `ncgo.config_get` / `ncgo.config_set` pair gated by
the `config.read` / `config.write` capabilities, with keys auto-namespaced
under `plugin.<plugin_id>`. Phase 4a registered them as stubs returning
`ErrUnsupported` after a coarse capability check. This increment lands the
real implementation, and — because plugin config is just one tenant of
app-scoped configuration — introduces a generic appconfig store (the
Nextcloud `oc_appconfig` analogue) that core and a future Admin UI can
reuse.

## Decision

1. **Generic appconfig table, not a plugin-only table.** Migration
   `0017_appconfig` creates `appconfig(appid, configkey, configvalue)` with
   `PRIMARY KEY (appid, configkey)` in all three dialects (TEXT columns in
   SQLite/Postgres; MySQL uses `VARCHAR(191)` keys to stay under the InnoDB
   index limit and TEXT for the value, matching how 0015/0016 sized key and
   payload columns). The new `internal/appconfig` package exposes
   `Store.Get/Set/Delete` over `database.DB`, with a dialect-aware upsert
   mirroring the plugin registry (MySQL `ON DUPLICATE KEY UPDATE`, SQLite/
   Postgres `ON CONFLICT ... DO UPDATE`) and an `ErrNotFound` sentinel on
   missing rows. Nothing in the schema or store is plugin-specific.

2. **Namespace scheme.** Plugin config rows live under `appid = "plugin"`;
   the stored key is `<plugin_id>.<key>` — the physical realisation of the
   spec's `plugin.<plugin_id>.` namespace. Cross-plugin reads are
   impossible even for a plugin granted `*`: its `foo` resolves to
   `<its-own-id>.foo`. Capability globs match the plugin-local key
   (`tokens.*` grants `tokens.x`, not the namespaced form), so manifests
   never mention plugin ids.

3. **Key and value constraints.** Keys are unrestricted byte strings
   except empty (rejected with `ErrInvalidArgument`); dots in keys are
   harmless because namespacing is by prefix, not parsing. Values are
   capped at `maxStringArg` (64KiB) on set (`ErrCodeTooLarge` beyond) —
   config is for settings, not blob storage; the 1 MiB payload cap on
   `readBytes` stays as a second line of defence. `config_get` writes the
   value through `writeBytes`, so a too-small guest buffer yields
   `ErrCodeTooLarge` with nothing written.

4. **Missing-key semantics.** A get for a key the plugin may read but that
   has no row returns the established `ErrCodeNotFound` (-4), the same
   code abi_cache/abi_storage use for misses — no new code, and guests can
   share miss handling across subsystems.

5. **Check order and defaults.** Both functions read the key, reject empty
   (-2), check the per-key capability glob (-3), then require a configured
   store (-12 `ErrCodeUnavailable`, keeping the default-deny-then-
   unavailable posture of the other host families). `HostConfig` gains
   `AppConfig *appconfig.Store`; app.go wires `appconfig.NewStore(a.DB)`.
   pluginsdk gains `ConfigGet(key, out)` / `ConfigSet(key, val)` bindings
   with non-wasm stubs.

## Alternatives Considered

### Plugin-only `plugin_config` table
- Pros: uninstall cascade is one `DELETE WHERE plugin_id = ?`; no appid
  column to filter.
- Cons: core settings and the Admin UI would need a second, near-identical
  table and store later anyway; Nextcloud parity (`oc_appconfig`) favours
  the generic shape. Uninstall cleanup is a follow-up `DELETE WHERE
  appid = 'plugin' AND configkey LIKE '<id>.%'` — cheap and explicit.

### Returning empty string instead of -4 on missing keys
- Pros: no error path for unset optional settings.
- Cons: conflates "unset" with "set to empty" (the store round-trips empty
  values), and diverges from the cache/storage miss convention. -4 keeps
  the semantics unambiguous.

### Larger value cap (1 MiB, matching the payload cap)
- Pros: fewer surprising -11s.
- Cons: config rows are read on hot paths and should stay small; 64KiB is
  generous for settings and keeps MySQL TEXT addressing trivial. A future
  `config_set_bytes`/storage handoff can serve larger payloads.

## Consequences

- The appconfig table is additive; existing installs migrate forward with
  `0017_appconfig`. No uninstall cascade yet: removing a plugin leaves its
  config rows behind (harmless, and preserves settings across reinstall).
- Follow-ups: a `config_delete` host function (spec lists none yet), a
  `config_keys`/list function for discovery, uninstall-time row cleanup,
  and encryption-at-rest for secret values (the table stores plaintext;
  plugins needing secrecy should combine config with a host-side secret
  store later).

## Verification

- End-to-end through wasm guests: granted plugin set→get round-trip with
  the value logged and the physical row confirmed as
  `plugin / <plugin_id>.<key>`; missing key → -4; read without a grant →
  -3; write outside granted globs (`tokens.*` vs `other`) → -3; nil store
  → -12; empty key → -2; value > 64KiB → -11; too-small get buffer → -11.
- Namespace isolation: plugin A sets `foo`; plugin B (granted `*`) getting
  `foo` → -4, and the store holds only `com.example.a.foo`.
- appconfig store unit tests: set/get/overwrite/empty value/delete/missing,
  appid isolation (sqlite).
- `canConfigRead`/`canConfigWrite` glob table tests, including nil
  capabilities.
- Migration up/down/up bumped to v17 with table assertions.
- `go test ./...` green, race clean, golangci-lint 0 issues.
