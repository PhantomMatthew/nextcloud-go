# ADR-0002: Greenfield Rewrite (No PHP Coexistence)

- **Status**: 🟢 Accepted
- **Date**: 2026-04-29
- **Deciders**: Project lead

## Context

Nextcloud Server is a mature PHP codebase (~500K LOC across `lib/private`, `lib/public`,
and `apps/`). Any rewrite must answer one foundational question before anything else:

> **Will the Go server coexist with the PHP server (sharing the database, runtime, and
> filesystem layout) — or will it be a clean greenfield with its own schema, data
> contracts, and migration story?**

The answer governs every downstream decision: schema design, ORM/query strategy, file
storage layout, encryption envelope format, app/plugin architecture, deployment topology,
upgrade/rollback semantics, and the entire test strategy.

### Constraints

- The Nextcloud PHP schema (the `oc_*` tables) was grown organically over 10+ years. It
  reflects PHP idioms (serialized PHP arrays in TEXT columns, mixed-case column naming,
  Doctrine DBAL portability quirks, app-specific tables interleaved with core).
- The PHP runtime owns invariants the schema alone does not express (filecache ETag
  propagation, share resolution, encrypted filename mapping, locking semantics).
- We are wire-compatible with **clients** (desktop, iOS, Android, CalDAV/CardDAV) — not
  with the PHP server's internal data model.
- Single-developer/small-team project; coexistence multiplies maintenance burden.

### Forces

- **Compatibility pressure**: existing Nextcloud users have data they want to keep.
- **Velocity pressure**: shipping a feature-parity Go rewrite is already a ~24-month
  effort; coexistence would inflate that significantly.
- **Correctness pressure**: any shared-DB approach risks cross-runtime corruption (two
  servers writing the same `oc_filecache` rows with different invariants).

## Decision

**`nextcloud-go` is a greenfield Go rewrite. It does NOT share a database, schema,
filesystem layout, or runtime with the PHP Nextcloud Server.**

Concretely:

1. **Fresh schema.** New table names (no `oc_` prefix), Go-idiomatic column naming,
   normalized JSON/JSONB columns instead of serialized PHP, explicit foreign keys,
   modern indexing strategy. Schema lives in `internal/db/migrations/` and is owned
   end-to-end by `nextcloud-go`.
2. **Fresh storage layout.** File storage paths, chunk upload temp layout, preview
   cache structure, and trashbin organization are designed for the Go server's
   storage abstraction — not bug-for-bug compatible with PHP's `data/` directory.
3. **No simultaneous operation.** A given user/instance is served by **either** PHP
   Nextcloud **or** `nextcloud-go` — never both pointed at the same data.
4. **Wire compatibility, not data compatibility.** The Go server speaks the same
   HTTP/WebDAV/OCS/OCM protocols clients expect. Internally, it stores and indexes
   data however it likes.
5. **Migration is a one-way, offline tool.** A separate `ncgo-cli import-nextcloud`
   command — designed in **Phase 4**, not earlier — reads a PHP Nextcloud installation
   (DB dump + data dir) and produces a populated `nextcloud-go` instance. Source
   instance is left untouched; result is a clean, validated Go deployment.

## Alternatives Considered

### Option A: Shared-database coexistence (Go server reads/writes PHP `oc_*` tables)

- **Pros**:
  - Users could "try" the Go server pointed at their existing data with no migration
    step.
  - Incremental rollout: certain endpoints served by Go, others still PHP.
  - No data-migration tooling needed.
- **Cons**:
  - We inherit every PHP schema quirk forever — serialized arrays, mixed naming,
    Doctrine DBAL portability hacks, decade-old denormalizations.
  - Two runtimes with different invariants mutating the same rows = corruption risk.
    Filecache ETag propagation alone is non-trivial; getting the Go side wrong
    silently breaks desktop sync.
  - Locking semantics differ between PHP (per-request) and Go (long-lived process,
    goroutines). Sharing the DB-level lock tables is a recipe for deadlocks.
  - Plugin/app system would have to interop with PHP `OCP\AppFramework` — impossible
    without embedding PHP.
  - Schema migrations become a coordination nightmare (which runtime owns them?).
  - Effectively forecloses ever modernizing the schema.
- **Verdict**: Rejected. The risk/maintenance cost vastly exceeds the migration-UX
  benefit, and the upper bound on architectural quality is permanently capped at
  "PHP Nextcloud, but Go".

### Option B: Strangler-fig (Go server proxies to PHP for unimplemented endpoints)

- **Pros**:
  - Could ship "something" earlier — Go handles WebDAV, PHP handles everything else.
  - Gradual cutover endpoint by endpoint.
- **Cons**:
  - Requires running both runtimes side-by-side indefinitely (operational complexity
    explosion: two language runtimes, two dependency trees, two security update
    cadences, two log formats).
  - Session/auth coordination across runtimes is hard (shared session store? token
    federation? OIDC bridge?).
  - Coexistence problems of Option A still apply for any shared state.
  - Negates the "single static binary" deployment story.
  - Users already have a working PHP Nextcloud; nobody benefits from a hybrid
    deployment unless the Go side is *also* a complete server — at which point the
    proxy layer is dead weight.
- **Verdict**: Rejected. Strangler-fig works when the legacy and new systems can be
  meaningfully isolated by domain. WebDAV, OCS, sharing, and auth in Nextcloud are
  too tightly coupled for clean endpoint-by-endpoint cutover.

### Option C: Schema-compatible greenfield (new code, but read-compatible with `oc_*`)

- **Pros**:
  - Could pivot to coexistence later if needed.
  - Lower migration friction (data is already in the right tables).
- **Cons**:
  - All the schema-quality downsides of Option A.
  - "Could pivot to coexistence later" is a feature nobody asked for; designing for
    optionality you'll never use is overhead with no payoff.
  - Forces us to reverse-engineer PHP's schema invariants exactly — a research task
    measured in months — before we can write the first line of business logic.
- **Verdict**: Rejected. We get all the constraint, none of the benefit.

### Option D (chosen): Pure greenfield, separate schema, offline migration tool

- **Pros**:
  - Schema is designed for Go idioms, modern Postgres/MySQL features, and a clean
    plugin model from day one.
  - No cross-runtime corruption risk — single source of truth at any time.
  - Migration is a clean, testable, well-defined transformation: PHP DB+data → Go
    DB+data, with a validation step.
  - Single static binary deployment story holds.
- **Cons**:
  - Users must explicitly migrate; no "just point it at your existing install"
    experience until the import tool ships in Phase 4.
  - We have to build and validate the import tool (estimated ~1 engineer-month in
    Phase 4 plus an extended QA cycle against real-world dumps).
  - "Wire-compatible but data-incompatible" is a subtle distinction users will
    misunderstand. Documentation burden.
- **Verdict**: **Accepted.** The schema/architecture freedom is worth the deferred
  migration UX, and the offline migration model is well-understood territory.

## Consequences

### Positive

- Schema and storage layout are designed for the actual workload, not constrained by
  legacy PHP decisions. Frees us to use JSONB, partial indexes, generated columns,
  proper FKs, and modern transaction isolation patterns.
- Clean plugin model: plugins target our stable contracts in `pkg/api/` and
  `pkg/pluginsdk/`, never the PHP `OCP\` interfaces.
- Single deployment artifact. No "PHP runtime + Go binary" hybrid to operate.
- Test strategy is tractable: fixture-based tests against our own schema, plus
  client-recorded golden HTTP captures for wire compatibility. No need for a PHP
  Nextcloud reference instance in CI.
- Performance ceiling is set by the Go runtime and our designs, not by replicating
  PHP's per-request lifecycle in a long-lived process.

### Negative

- **No "drop-in replacement" UX until Phase 4.** Early adopters running Phase 1–3
  builds must accept "fresh install only".
- **Migration tool is a separate, substantial workstream.** Real-world Nextcloud
  installs include thousands of edge cases (corrupt rows, app-specific tables for
  apps we don't support, orphaned shares, encrypted files with lost keys). We will
  have to triage and document what the import tool handles vs. what it skips/warns
  on.
- **Users with custom PHP apps cannot bring them.** Their workflows must be replaced
  by built-in features, WASM plugins, or external integrations. This is a hard
  message and must be communicated clearly in user-facing docs.
- **We lose the safety-net option of "fall back to PHP if a Go endpoint is buggy".**
  Wire compatibility must be airtight before any user migrates.

### Neutral / follow-ups

- The import tool's design (ADR forthcoming in Phase 3) will need to specify:
  schema mapping rules, handling of unsupported apps' tables, encryption key
  migration, share-link URL preservation, and rollback semantics.
- A separate "compatibility report" tool may be useful pre-migration: scan a PHP
  Nextcloud install and report which features/apps the user depends on that
  `nextcloud-go` does not yet implement.
- This decision implicitly forecloses a "hybrid mode for federation between PHP
  and Go instances" — which is fine; OCM (Open Cloud Mesh) federation handles that
  case at the protocol level, exactly as it would between two unrelated servers.

## References

- [`docs/plans/00-phased-rewrite-plan.md`](../plans/00-phased-rewrite-plan.md) —
  phased roadmap; migration tool sits in Phase 4.
- [`docs/architecture/overview.md`](../architecture/overview.md) — system
  architecture assumes greenfield.
- [Open Cloud Mesh (OCM)](https://cs3org.github.io/OCM-API/docs.html) — federation
  protocol; how Go and PHP instances would interoperate at the network level.
- Martin Fowler — [Strangler Fig
  Application](https://martinfowler.com/bliki/StranglerFigApplication.html) (the
  pattern we explicitly rejected, with reasoning above).
