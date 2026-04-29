# Architecture Overview

**Status**: 🟢 Accepted
**Last updated**: 2026-04-29

## High-Level View

```
                    ┌────────────────────────────────────────────┐
                    │            Existing Nextcloud Clients       │
                    │  Desktop • iOS • Android • CalDAV • CardDAV │
                    └─────────────────────┬──────────────────────┘
                                          │ HTTPS
                                          │ (wire-compatible)
                    ┌─────────────────────▼──────────────────────┐
                    │              ncgo (single binary)            │
                    ├──────────────────────────────────────────────┤
                    │  internal/httpx     chi router + middleware  │
                    │  ┌──────────────────────────────────────┐   │
                    │  │ /remote.php/dav/* → internal/webdav  │   │
                    │  │ /ocs/v{1,2}.php/* → internal/ocs     │   │
                    │  │ /index.php/login/* → internal/auth   │   │
                    │  │ /apps/<plugin>/*  → internal/plugin  │   │
                    │  └──────────────────────────────────────┘   │
                    │                                              │
                    │  internal/auth      internal/jobs            │
                    │  internal/storage   internal/eventbus        │
                    │  internal/cache     internal/plugin (wazero) │
                    │  internal/db        internal/obs             │
                    └─────────────┬────────────┬──────────────────┘
                                  │            │
                ┌─────────────────┘            └────────────────┐
                │                                                │
       ┌────────▼────────┐  ┌──────────┐  ┌──────────┐  ┌──────▼──────┐
       │  PostgreSQL /   │  │  Redis   │  │  S3 /    │  │  WASM       │
       │  MySQL /        │  │  (L2     │  │  Local   │  │  Plugins    │
       │  SQLite         │  │  cache)  │  │  FS      │  │  (.ncplugin)│
       └─────────────────┘  └──────────┘  └──────────┘  └─────────────┘
```

## Core Principles

1. **Single static binary.** `go build` produces one executable. No CGO. Cross-compile
   to any supported platform.
2. **Stateless server, stateful infrastructure.** All state in DB, cache, or storage —
   never in process memory beyond per-request scope. Horizontal scaling is "run more
   binaries behind a load balancer."
3. **Capability-based plugin sandbox.** Third-party code runs as WASM with explicit
   declared capabilities. Default deny.
4. **Wire compatibility is a hard contract.** Validated via captured-traffic golden
   tests, not aspirational documentation.
5. **Interfaces frozen early, implementations iterate.** The `internal/{db,storage,cache,auth,jobs,plugin}`
   interfaces freeze in Phase 0; concrete implementations evolve through phases.

## Subsystem Map

### `internal/httpx` — HTTP Server

- chi router; mounts subrouters per protocol (WebDAV, OCS, login, plugin routes)
- Middleware chain: request ID → recovery → access log → tracing → auth → CORS
- Centralized error formatting (matches Nextcloud's `{ocs:meta,...}` and WebDAV
  `<d:error>` shapes)

### `internal/ocs` — OCS API

- Envelope encoder/decoder (XML and JSON variants per OCS spec)
- Capability registry: subsystems contribute capabilities at startup; merged into
  `/cloud/capabilities` response
- Routing for OCS v1 and v2 endpoints

### `internal/webdav` — WebDAV Core

- Forked from `golang.org/x/net/webdav` (Phase 1 will diverge significantly)
- Custom property registry: subsystems and plugins register `oc:`/`nc:` properties
- ETag computation matching Nextcloud's algorithm (must replicate filecache propagation)
- Lock manager (single-node only in v1)

### `internal/auth` — Authentication

- `Authenticator` interface: Basic, Bearer (app password), Session cookie
- `SessionStore`: persisted in DB
- App password lifecycle (issue, list, revoke)
- Login flow v2 (`/index.php/login/v2/poll`, `/index.php/login/v2/grant`)
- Password hashing: Argon2id (configurable parameters)

### `internal/storage` — Storage Backends

- `Storage` interface; backends pluggable via configuration
- `localfs` — filesystem with sharded layout (Phase 1)
- `s3` — S3-compatible object storage with multipart upload (Phase 2)
- Streaming-first: never buffer whole files in memory

### `internal/db` — Database

- `DB`/`Tx`/`Querier` interfaces over `database/sql` patterns (but typed)
- Drivers: pgx (Postgres), go-sql-driver/mysql, modernc.org/sqlite
- Query construction via squirrel (NO ORM)
- Migrations via golang-migrate; embedded in binary

### `internal/cache` — Tiered Cache

- L1: ristretto (in-process, GC-friendly LRU with TinyLFU admission)
- L2: Redis (shared across replicas)
- Get path: L1 → L2 → miss callback → backfill both
- Set path: write-through to L2, async warm to L1

### `internal/jobs` — Background Jobs

- Persistent queue in `jobs` table (DB-as-queue; sufficient for Phase 0–4)
- Workers poll for `run_at <= NOW()` rows, claim with row-level lock
- Cron-style scheduled jobs registered at startup
- Plugins enqueue via host function (Phase 4)

### `internal/eventbus` — In-Process Event Bus

- Topic-based pub/sub
- Synchronous in-process delivery (Phase 0–3)
- Plugin subscriptions delivered via plugin host (Phase 4)
- No cross-process eventing in v1 (acceptable for single-binary deployments)

### `internal/plugin` — WASM Plugin Host

- wazero runtime, no WASI
- Manifest-driven (`plugin.toml`)
- Capability enforcement on every host call
- Per-plugin instance pools (configurable model: per-request / pooled / singleton)
- See [`../specs/wasm-plugin-abi.md`](../specs/wasm-plugin-abi.md)

### `internal/obs` — Observability

- Structured logging via `log/slog` (JSON in prod, text in dev)
- Prometheus metrics: HTTP, DB pool, cache hit/miss, job queue, plugin host
- OpenTelemetry tracing: HTTP spans propagated to DB, cache, storage, plugin calls
- Health endpoints: `/status.php`, `/livez`, `/readyz`

## Module System (First-Party In-Tree)

`modules/` contains first-party features that ship in-binary but follow plugin-style
isolation conventions. Examples: `modules/files-trash/`, `modules/sharing/`,
`modules/dav-caldav/`.

A module:
- Implements the `Module` interface from `pkg/api`
- Registers HTTP routes, OCS endpoints, WebDAV properties via Host functions
- Subscribes to events
- Owns its DB tables (defined in `migrations/`, namespaced by module ID)

In Phase 4, selected modules become reference WASM plugins (proving the plugin ABI
is sufficient for real workloads).

## Data Flow Examples

### Read: Desktop client browses a folder

```
Client PROPFIND /remote.php/dav/files/alice/Documents/
  → httpx middleware: request ID, auth (Bearer app_password)
    → auth.AuthenticateBearer → Identity{user_id: alice}
  → webdav.PropFindHandler
    → cache.Get("propfind:alice:/Documents/:depth1") MISS
    → storage.List(ctx, "alice/Documents/")
      → localfs reads directory entries
    → db.Query("SELECT custom props FOR these paths")
    → assemble multistatus XML response
    → cache.Set(..., 30s)
  → httpx writes 207 Multi-Status
```

### Write: Desktop client uploads a file

```
Client PUT /remote.php/dav/files/alice/photo.jpg
  → httpx middleware (auth, request ID)
  → webdav.PutHandler
    → quota check (db lookup)
    → storage.Create(ctx, "alice/photo.jpg", contentLength)
    → stream body → storage writer
    → compute ETag
    → db.Tx: insert/update file row, propagate ETag up parent chain
    → eventbus.Publish("files.uploaded", {path, user, size})
      → plugin host fans out to subscribers (Phase 4)
  → 201 Created with ETag header
```

## Dependency Direction

```
cmd/         depends on    →    internal/* and pkg/*
internal/app depends on    →    all other internal/*
internal/X   depends on    →    pkg/* and stdlib only (NOT other internal/X)
modules/X    depends on    →    pkg/api (NOT internal/*)
pkg/api      depends on    →    stdlib only
pkg/pluginsdk depends on   →    stdlib only (compiled to WASM)
```

The `internal/` packages cross-talk only through interfaces defined in `pkg/api`,
preventing cyclic imports and forcing clean abstraction boundaries.

## Deployment Topologies

**Single-node (default)**: one `ncgo` binary + Postgres + Redis + filesystem.

**Multi-node** (Phase 4 supported): N `ncgo` behind load balancer + Postgres
(primary/replica) + Redis (cluster) + S3-compatible storage. Sticky sessions not
required (sessions stored in DB).

**Single-binary embedded** (dev/demo): `ncgo` with embedded SQLite + local FS,
no Redis. `--config minimal.yaml` flag.

## Change Log

- **2026-04-29** — Initial overview committed.
