# ADR-0058: Phase 4m cache.DeleteByPrefix — plugin uninstall cache cleanup

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Resolves**: ADR-0056 Deferred item 1 (cache-key cleanup on uninstall)

## Context

Phase 4j (ADR-0056) made `Installer.Uninstall` remove everything a plugin
persists — route/prop records, jobs rows, appconfig rows, the system-storage
tree — with one documented exception: cache keys under `plugin:<id>:`.
`cache.Cache` only had `Get`/`Set`/`Delete`/`Increment`, so there was no way
to delete by namespace. The residue is harmless in a memory-only deployment
(the cache dies with the process, and uninstalls take effect on restart
anyway), but a Redis-backed L2 survives restarts: a reinstalled plugin would
read the previous generation's keys.

## Decision

1. **Interface**: `Cache` gains
   `DeleteByPrefix(ctx, prefix string) (int64, error)`, returning the number
   of deleted keys (for uninstall observability). An **empty prefix is
   rejected** (`ErrEmptyPrefix`) — it would otherwise compile to a Redis
   `MATCH *` and wipe the whole keyspace. All three implementations enforce
   the guard.

2. **Memory (ristretto)**: ristretto has no key-iteration API, and its
   eviction callbacks report only the hashed key (`Item.Key uint64`), so
   `Memory` now stores a `memoryEntry{key, val}` — the original string key
   travels with the value — and maintains a `keys` index (sharing the
   `Increment` mutex). `Set` tracks the key *before* entering ristretto's
   set buffer; `OnEvict` (policy evictions and the periodic TTL cleanup)
   and `OnReject` (admission refusal) untrack it, and the buffer-drop error
   path untracks manually, so the index mirrors the store. `DeleteByPrefix`
   collects matches from `keys` and the `inc` counter map under the mutex,
   then `Del`s outside it — the callbacks run on ristretto's background
   goroutine, and holding the mutex across a cache call (`Set`/`Wait`)
   would deadlock with a callback blocking on the same mutex. A TTL-expired
   key may linger in the index until the cleanup ticker reports it; the
   resulting `Del` is a harmless no-op and the key is still counted.

3. **Redis**: a SCAN cursor loop (`MATCH` = the prefix with the glob
   metacharacters `\ * ? [ ]` backslash-escaped by the pure
   `redisMatchPattern` helper, plus a trailing `*`; `COUNT 200`) with
   batched **UNLINK** instead of DEL — UNLINK (Redis ≥ 4) defers the actual
   free to a background thread, so a large plugin keyspace never blocks the
   server.

4. **Tiered**: both layers are swept; the returned count is L2's when an L2
   is present (the authority), else L1's — the layers mirror the same
   keyspace, so summing would double-count. Error handling mirrors
   `Tiered.Delete` (L1's error wins).

5. **Uninstall wiring**: `Installer` gains a `Cache cache.Cache` field (nil
   skips the step, like the other cleanup dependencies). `Uninstall` runs
   `DeleteByPrefix(ctx, "plugin:<id>:")` right after the appconfig step —
   the prefix helper `cacheKeyPrefix(id)` is shared with `abi_cache.go`'s
   `cacheKey` so the write and delete paths cannot drift — and logs the
   deletion count when non-zero. The stale "cache keys are NOT cleaned"
   comments were corrected.

6. **CLI wiring**: `ncgo-cli plugin uninstall` builds a `cache.Redis` from
   `cache.redis_*` config **only when `cache.redis_addr` is set** and
   injects it into the Installer. A CLI-local Memory would be an empty
   shell (it starts empty and dies with the CLI process), and memory-only
   residue dies with the server on the restart uninstalls already require —
   the same semantics as enable/disable. The install host's `Cache` stays
   deliberately nil (ADR-0056 G1): `cache_*` hooks in the CLI still get -12.

## Consequences

- Uninstalling a plugin from a Redis-backed deployment no longer leaves
  `plugin:<id>:*` keys for a future reinstall to read; the CLI reports the
  purged count in its log.
- The interface widening touches exactly three implementations (Memory,
  Redis, Tiered) — the compiler confirms there is no fourth.
- `Memory` values grow by one string header per entry (the key is shared,
  not copied).
- `DeleteByPrefix` is O(keyspace / COUNT) round trips on Redis and
  O(tracked keys) on Memory; both are management-path operations, not
  request-path.
- No new dependencies (SCAN/UNLINK are go-redis built-ins; no miniredis);
  `go mod tidy` is a no-op.

## Verification

- `internal/cache`: `TestMemoryDeleteByPrefix` (match/no-match, prefix
  boundary `plugin:a:` vs `plugin:ab:`, counter sweep, count, empty-prefix
  rejection, post-delete `ErrMiss`), `TestMemoryDeleteByPrefixExpiredKey`
  (TTL-expired key still swept), `TestMemoryEvictionUntracks` (index mirrors
  the store under eviction pressure), `TestRedisMatchPattern` (table test
  including `a*b?c[d]\e`), `TestTieredDeleteByPrefix` (both layers swept,
  L2-authoritative count), and a `DeleteByPrefix` section in the
  integration-tagged Redis test (`NCGO_TEST_REDIS_ADDR`; skipped by default).
- `internal/plugins`: `TestUninstallCleanup` now presets
  `plugin:com.example.inst:foo`, a `plugin:com.example.inst2:bar` boundary
  control, and a counter — uninstall sweeps the plugin's keys and leaves
  the control.
- Gates: golangci-lint 0 issues, gofumpt/gofmt clean, `go test ./...` and
  `go test -race` green, `go vet -tags integration ./internal/cache/`
  compiles the Redis test without a server.
