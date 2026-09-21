# ADR-0039: Phase 4c1 Plugin db.* Host Functions & SQL Safety

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Spec §6.3 defines handle-based `db.*` host functions guarded by
`db.read`/`db.write` table globs, with SQL parsed and allowlisted before
execution. Spec §14 Q1 left the parser choice open: `pg_query_go` is
Postgres-only, and the project supports sqlite/postgres/mysql.

## Decision

1. **Parser: `github.com/xwb1989/sqlparser`** (the compact vitess fork).
   Resolves spec §14 Q1 for v1: one MySQL-dialect parser for all three
   backends; plugin SQL is expected to stay in the portable standard subset.
   Fail closed: any parse error or unsupported statement class is rejected
   before execution. Table extraction walks the full AST so subqueries in
   WHERE/SELECT are covered; `ColName` nodes are not descended into, so
   column qualifiers/aliases are not mistaken for table references.

2. **Statement classes.** SELECT → every table must match `db.read`;
   INSERT/UPDATE/DELETE → `db.write`; DDL (CREATE/ALTER/DROP/RENAME) →
   `db.write` **and** lifecycle-hook context only (spec §6.3: no DDL
   outside install/uninstall hooks). SET/SHOW/USE/multi-statement →
   denied. `db_query` rejects writes and vice versa with
   `ErrInvalidArgument`.

3. **Hook context.** `Plugin.callEntry` marks install/uninstall/upgrade
   calls with an `inHook` flag in the per-call `callInfo`; plain `Call`
   (request/event/job paths) never sets it.

4. **Rows & encoding.** Rows are MessagePack arrays (spec §6.1) via
   `vmihailenco/msgpack/v5` (no codegen, works under wasip1); `time.Time`
   normalizes to RFC3339Nano strings. `db_rows_next` returns bytes
   written, 0 = EOF. Packed i64 returns: high 32 = error code, low 32 =
   handle. `database.Rows` gained `Columns()` (already satisfied by
   `*sql.Rows`).

5. **Handles.** Rows and transactions share the 16-handle rows budget.
   Every release path (clean, broken, shutdown) now closes leftover
   handles: rows closed, open transactions rolled back — plugins that
   forget `db_rows_close` cannot leak cursors across pooled reuse.

6. **Host wiring.** `HostConfig.DB database.DB`; nil → `ErrUnavailable`.
   The app passes its main DB. Plugins share the server's connection pool
   in v1 (per-plugin pools are a future hardening option).

## Alternatives Considered

### pg_query_go + vitess + custom SQLite shim (per dialect)
- Pros: dialect-exact parsing.
- Cons: three parsers to vet and keep aligned; pg_query_go drags in
  libpg_query; the allowlist gate needs table names, which all three
  dialects express identically in the portable subset.

### No raw SQL — structured query API only
- Pros: no parser risk at all.
- Cons: contradicts the spec (§6.3 defines raw SQL + parser), and forces
  plugins into a lowest-common-denominator query builder.

## Consequences

- xwb1989/sqlparser is a 2018-era fork: it lacks CTEs and some modern
  syntax. Plugin SQL using those fails closed (parse error). If a
  maintained parser matures (vitess subpackage isolation, sqlc
  ecosystem), swapping is localized to `sqlparse.go`.
- The allowlist is the security boundary; parser blind spots = security
  holes. The full-AST walk + fail-closed posture is deliberate; any
  future parser swap must preserve both.
- Follow-ups: per-plugin connection pools/quotas, query duration
  accounting into spec §12 metrics, read/write split routing.

## Verification

- sqlparse unit tests: classification, joins, subqueries, aliases,
  multi-statement and garbage rejection, glob enforcement.
- End-to-end on real sqlite through wasm guests: hook DDL + insert +
  select round-trip (row logged), EOF signaling, handle close, tx
  begin/exec/query/commit, rollback invalidating the handle (-4),
  denied table (-3), write-via-query rejected (-2), multi-statement
  rejected (-2), nil DB (-12).
- `go test ./...` green, race clean, golangci-lint 0 issues.
