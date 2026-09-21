# ADR-0037: Phase 4a Plugin Runtime Core

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

The Phase 0 plugin stub (`internal/plugins`) exposed only `ncgo.log`, kept a
single instance per plugin regardless of `instance_model`, and left
`[capabilities]` as an unparsed `map[string]any`. The ABI spec
(`docs/specs/wasm-plugin-abi.md`) defines ~39 host functions and three
instance models; implementing everything in one increment is too large, and
the db.\* SQL-parser question (spec §14) is still open.

## Decision

1. **Full host surface registered, most functions stubbed.** All `ncgo.*`
   imports from spec §6.3 are registered at host start so guest modules
   linking any of them instantiate. Implemented now: `log`, `ctx_*`,
   `crypto_*` (sha256/sha512/BLAKE2b-256), `cache_*` (backed by
   `cache.Cache`, keys force-namespaced `plugin:<id>:`). Everything else
   (db/storage/http/events/jobs/routes/ocs/webdav/config) validates its
   capability first — returning `ErrPermissionDenied` on a missing grant —
   then `ErrUnsupported`. Security posture is exercised from day one.

2. **Typed capabilities.** `[capabilities]` decodes into nested structs
   (`Capabilities.DB.Read`, …) because go-toml/v2 maps TOML dotted keys to
   nested tables, not flat `db.read` tags. Validation: storage scopes are
   `user|system`; routes/ocs entries must start with `/apps/` (spec §5
   says `/apps/<plugin-id>/` but the spec's own walkthrough grants
   `/apps/file-tagger/` for id `com.example.file-tagger` — the looser
   prefix rule wins, noted in the spec change log); `events.publish`
   rejects `core.*`; webdav props reject `oc:`/`nc:`/`core.` prefixes.

3. **Instance manager.** `per_request` instantiates per call; `pooled`
   pre-warms `pool_size` instances at Install and traps replenish from a
   detached context; `singleton` is mutex-serialized. Any trap destroys
   the instance (spec §8). Instance names are `<id>#<seq>` because wazero
   rejects duplicate module names in one runtime. Per-instance handle
   tables (stream 64 / rows 16 / http 16) ship now but have no producers
   until the db/storage/http increments.

4. **CallContext plumbing.** `WithCallContext` attaches user id, request
   id, locale, deadline for the next `Plugin.Call`; `ctx_*` host functions
   read it. `Plugin.Call` returns raw results — non-zero i32 is only an
   error by entry-point convention (`callEntry` converts), since
   `ncgo_abi_version` legitimately returns 1.

5. **pluginsdk bindings.** `CtxUserID/RequestID/Locale/DeadlineUnixMS`,
   `CryptoRandom/Hash/HMAC`, `CacheGet/Set/Delete/Increment` under
   `//go:build wasm`, with `!wasm` no-op stubs so host code can import the
   package.

6. **Tests without TinyGo.** `wasmgen` was generalized (import table,
   multiple data blobs, extra exports, mutable globals, if/drop/i64 ops)
   to emit probe modules: ctx round-trip, cache round-trip + increment,
   crypto digest length, capability denied vs granted-unsupported probes.

## Alternatives Considered

### Implement the full ABI in one increment
- Pros: no ErrUnsupported stubs.
- Cons: db.\* needs the unsettled SQL-parser choice; event bus and route
  mounting don't exist yet; review surface too large.

### Flat capabilities via custom UnmarshalTOML
- Pros: keeps `DBRead []string` ergonomics.
- Cons: hand-rolled decoding for one section duplicates go-toml's dotted
  key handling for no behavioral gain.

## Consequences

- Modules linking the full ABI instantiate today and get deterministic
  `ErrPermissionDenied`/`ErrUnsupported` instead of link failures.
- Follow-up increments (each its own ADR): db.\* with SQL parser + table
  allowlist, storage streaming, outbound HTTP with IP checks, event bus,
  jobs wiring, route/ocs mounting, plugin config store, Prometheus
  metrics (spec §12), ed25519 signing (Phase 4b).
- `HostConfig.Cache` is wired from `app.Cache`; nil cache makes `cache_*`
  return `ErrUnavailable`.

## Verification

- `go test ./...` green; `go test -race` green; golangci-lint 0 issues;
  coverage 67.5% → 68.9%.
- New tests: capability glob/prefix matching, manifest capability parse +
  validation, instance models (per_request fresh / singleton shared /
  pooled concurrent / trap replenish), handle table limits and kind
  isolation, host function probes through real wasm guests.
