# Phase 0 Blueprint — Planning & Foundations

**Status**: 🟢 Accepted
**Duration**: 8–10 weeks
**Team**: 2 engineers
**Last updated**: 2026-04-29

## Goal

Boot a `ncgo` binary, lock the architecture, prove the contract-testing approach,
and ship the WASM plugin host stub. **No business logic.**

## Repository Layout

```
nextcloud-go/
├── cmd/
│   ├── ncgo/                 Main server binary
│   ├── ncgo-cli/             Operator CLI (occ-equivalent)
│   └── ncgo-captest/         Contract test runner (replays golden HAR files)
├── internal/
│   ├── app/                  Application bootstrapping, dependency wiring
│   ├── config/               koanf-based configuration loading
│   ├── db/                   DB abstraction, migrations, query builders
│   ├── cache/                L1 (ristretto) + L2 (Redis) cache facade
│   ├── storage/              Storage backend abstraction (local, S3)
│   ├── auth/                 Authentication, sessions, app passwords, login flow v2
│   ├── httpx/                HTTP server, middleware, error formatting
│   ├── ocs/                  OCS API envelope, routing, capability registry
│   ├── webdav/               WebDAV core, custom property registry
│   ├── jobs/                 Background job framework
│   ├── eventbus/             In-process event bus
│   ├── plugin/               WASM plugin host (wazero)
│   └── obs/                  Observability (slog, prometheus, otel)
├── pkg/
│   ├── api/                  Stable public Go API for in-tree modules
│   └── pluginsdk/            Go bindings plugin authors compile against
├── modules/                  First-party in-binary modules (later become
│                             reference plugin implementations)
├── test/
│   ├── compat/               Wire-compatibility tests
│   ├── golden/               Captured request/response fixtures (HAR + decoded)
│   ├── fixtures/             Test data, seed users, seed files
│   ├── integration/          Multi-component tests with testcontainers-go
│   └── e2e/                  Full-stack tests against real clients
├── deploy/
│   ├── docker/               Dockerfile, docker-compose.dev.yml
│   ├── helm/                 Helm chart (Phase 4)
│   └── systemd/              systemd unit files
├── docs/                     (this directory)
├── tools/
│   ├── capture/              mitmproxy scripts for traffic capture
│   └── golden-gen/           Convert raw HAR → typed golden cases
├── go.mod
├── go.sum
├── Makefile
├── .golangci.yml
└── .github/workflows/        CI definitions
```

## Core Interfaces (Drafted in Phase 0)

These freeze in Phase 0 and define how every subsystem talks to every other.

### `internal/db/db.go`

```go
type DB interface {
    Querier
    Begin(ctx context.Context) (Tx, error)
    Close() error
    Ping(ctx context.Context) error
}

type Tx interface {
    Querier
    Commit() error
    Rollback() error
}

type Querier interface {
    Query(ctx context.Context, query string, args ...any) (Rows, error)
    QueryRow(ctx context.Context, query string, args ...any) Row
    Exec(ctx context.Context, query string, args ...any) (Result, error)
}
```

Concrete drivers under `internal/db/postgres/`, `internal/db/mysql/`, `internal/db/sqlite/`.

### `internal/storage/storage.go`

```go
type Storage interface {
    Stat(ctx context.Context, path string) (*FileInfo, error)
    Open(ctx context.Context, path string) (io.ReadSeekCloser, error)
    Create(ctx context.Context, path string, size int64) (io.WriteCloser, error)
    Delete(ctx context.Context, path string) error
    List(ctx context.Context, path string) ([]*FileInfo, error)
    Rename(ctx context.Context, src, dst string) error
}
```

Backends: `internal/storage/localfs/`, `internal/storage/s3/` (Phase 2).

### `internal/cache/cache.go`

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
    Delete(ctx context.Context, key string) error
    Increment(ctx context.Context, key string, delta int64) (int64, error)
}
```

Tiered implementation in `internal/cache/tiered/` (ristretto L1, Redis L2).

### `internal/auth/auth.go`

```go
type Authenticator interface {
    AuthenticateBasic(ctx context.Context, user, pass string) (*Identity, error)
    AuthenticateBearer(ctx context.Context, token string) (*Identity, error)
    AuthenticateSession(ctx context.Context, sessionID string) (*Identity, error)
}

type SessionStore interface {
    Create(ctx context.Context, identity *Identity) (*Session, error)
    Lookup(ctx context.Context, sessionID string) (*Session, error)
    Revoke(ctx context.Context, sessionID string) error
}
```

### `internal/jobs/jobs.go`

```go
type Job interface {
    Name() string
    Run(ctx context.Context, payload []byte) error
}

type Runner interface {
    Register(job Job) error
    Enqueue(ctx context.Context, name string, payload []byte, runAt time.Time) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

### `internal/plugin/host.go`

```go
type Host interface {
    Install(ctx context.Context, archive io.Reader) (*Manifest, error)
    Uninstall(ctx context.Context, pluginID string) error
    Invoke(ctx context.Context, pluginID, function string, payload []byte) ([]byte, error)
    PublishEvent(ctx context.Context, topic string, payload []byte) error
}

type Module interface {
    ID() string
    Init(ctx context.Context, host ModuleHost) error
    Routes() []Route
}
```

## Tech Stack (Locked — see ADR-0001)

| Concern | Choice | Rejected alternatives |
|---|---|---|
| HTTP router | `github.com/go-chi/chi/v5` | gin, echo, fiber, gorilla/mux |
| DI | Hand-rolled constructor injection in `internal/app/` | uber-go/fx, google/wire |
| Postgres driver | `github.com/jackc/pgx/v5` | lib/pq, database/sql std driver |
| MySQL driver | `github.com/go-sql-driver/mysql` | — |
| SQLite driver | `modernc.org/sqlite` (pure Go) | mattn/go-sqlite3 (CGO), crawshaw/sqlite (CGO) |
| Query builder | `github.com/Masterminds/squirrel` | gorm, ent, sqlc |
| Migrations | `github.com/golang-migrate/migrate/v4` | goose, atlas |
| Redis client | `github.com/redis/go-redis/v9` | gomodule/redigo |
| In-memory cache | `github.com/dgraph-io/ristretto` | bigcache, freecache |
| WebDAV core | Fork `golang.org/x/net/webdav` | sabre/dav (PHP), build from scratch |
| S3 client | `github.com/aws/aws-sdk-go-v2` + `github.com/minio/minio-go/v7` | rclone (overkill) |
| WASM runtime | `github.com/tetratelabs/wazero` (pure Go, no CGO) | wasmer-go (CGO), wasmtime-go (CGO) |
| Logging | `log/slog` (stdlib) | logrus, zap, zerolog |
| Metrics | `github.com/prometheus/client_golang` | — |
| Tracing | `go.opentelemetry.io/otel` | — |
| Config | `github.com/knadh/koanf/v2` | viper, envconfig |
| CLI framework | `github.com/spf13/cobra` | urfave/cli |
| Testing | `github.com/stretchr/testify` + `github.com/testcontainers/testcontainers-go` | — |

**Rejected globally**: gorm, ent, gin, echo, viper, uber-go/fx, any CGO dependency.

## YAML Configuration Schema (Phase 0)

```yaml
# /etc/nextcloud-go/config.yaml

server:
  listen: "0.0.0.0:8080"
  trusted_proxies: ["10.0.0.0/8"]
  trusted_domains: ["cloud.example.com"]
  base_url: "https://cloud.example.com"

database:
  driver: "postgres"   # postgres | mysql | sqlite
  dsn: "postgres://ncgo:secret@db:5432/ncgo?sslmode=disable"
  max_open_conns: 50
  max_idle_conns: 10

cache:
  l1_max_items: 100000
  l1_max_cost_mb: 256
  redis_addr: "redis:6379"   # optional; if empty, L1 only
  redis_db: 0

storage:
  default_backend: "local"
  backends:
    local:
      type: "localfs"
      root: "/var/lib/ncgo/data"
    s3_primary:
      type: "s3"
      endpoint: "https://s3.example.com"
      bucket: "ncgo-files"
      access_key_id: "${S3_ACCESS_KEY}"
      secret_access_key: "${S3_SECRET_KEY}"
      region: "us-east-1"

auth:
  session_ttl: "24h"
  app_password_ttl: "0"   # 0 = never expires
  password_hash: "argon2id"
  argon2id:
    memory_kb: 65536
    iterations: 3
    parallelism: 4

jobs:
  workers: 4
  poll_interval: "5s"

plugin:
  enabled: true
  install_dir: "/var/lib/ncgo/plugins"
  default_memory_limit_mb: 32
  default_cpu_timeout_ms: 5000

observability:
  log_level: "info"        # debug | info | warn | error
  log_format: "json"       # json | text
  metrics_listen: "127.0.0.1:9090"
  otel_endpoint: ""        # empty = disabled
```

## Initial DB Schema (`migrations/0001_init.sql`)

```sql
-- Users
CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    uid             TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    email           TEXT,
    password_hash   TEXT NOT NULL,
    quota_bytes     BIGINT,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_users_uid ON users(uid);

-- Groups
CREATE TABLE groups (
    id              BIGSERIAL PRIMARY KEY,
    gid             TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE group_members (
    group_id        BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

-- Sessions (browser/cookie)
CREATE TABLE sessions (
    id              TEXT PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_agent      TEXT,
    ip              INET,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- App passwords (Bearer tokens for desktop/mobile clients)
CREATE TABLE app_passwords (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    last_used_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ
);
CREATE INDEX idx_app_passwords_user ON app_passwords(user_id);
CREATE INDEX idx_app_passwords_token ON app_passwords(token_hash);

-- Background jobs
CREATE TABLE jobs (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    payload         BYTEA,
    run_at          TIMESTAMPTZ NOT NULL,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    last_error      TEXT,
    attempts        INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_jobs_run_at ON jobs(run_at) WHERE completed_at IS NULL;

-- Module/plugin configuration
CREATE TABLE module_config (
    module_id       TEXT NOT NULL,
    key             TEXT NOT NULL,
    value           BYTEA,
    PRIMARY KEY (module_id, key)
);
```

## Contract Test Harness

Wire compatibility cannot be designed — it must be **observed**. The harness:

1. **Capture** (`tools/capture/`)
   - Run reference Nextcloud PHP server in Docker
   - Real Nextcloud desktop / iOS / Android clients connect through mitmproxy
   - mitmproxy script writes HAR files per scenario into `test/golden/raw/`
   - Scenarios: login, capability fetch, sync (initial), sync (incremental), upload,
     chunked upload, download, share creation, calendar event create, contact create,
     federation handshake, etc.

2. **Convert** (`tools/golden-gen/`)
   - Parse raw HAR → typed `*.golden.json` files
   - Strip volatile headers (`Date`, `Request-Id`)
   - Tag each request with semantic name (e.g. `desktop-login-flow-v2-step3`)

3. **Replay** (`cmd/ncgo-captest/`)
   - Boot `ncgo` against test database
   - For each golden case: replay the request, diff response body & headers
   - Custom matchers for ETags, dates, UUIDs (regex equivalence, not exact)
   - Output: pass/fail per case, summary report, per-phase coverage rollup

This harness is **the** acceptance test for every wire-facing feature in Phases 1–4.

## Docker Compose Dev Environment

`deploy/docker/docker-compose.dev.yml`:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: ncgo
      POSTGRES_PASSWORD: ncgo
      POSTGRES_DB: ncgo
    ports: ["5432:5432"]
    volumes: ["pgdata:/var/lib/postgresql/data"]

  mysql:
    image: mysql:8
    environment:
      MYSQL_ROOT_PASSWORD: root
      MYSQL_DATABASE: ncgo
      MYSQL_USER: ncgo
      MYSQL_PASSWORD: ncgo
    ports: ["3306:3306"]

  redis:
    image: redis:7-alpine
    ports: ["6379:6379"]

  minio:
    image: minio/minio:latest
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: minio
      MINIO_ROOT_PASSWORD: minio12345
    ports: ["9000:9000", "9001:9001"]
    volumes: ["miniodata:/data"]

  # Reference Nextcloud PHP for capture
  reference-nextcloud:
    image: nextcloud:latest
    ports: ["8081:80"]
    environment:
      MYSQL_HOST: mysql
      MYSQL_DATABASE: nextcloud
      MYSQL_USER: nextcloud
      MYSQL_PASSWORD: nextcloud
    depends_on: [mysql]

  mitmproxy:
    image: mitmproxy/mitmproxy:latest
    command: mitmweb --web-host 0.0.0.0
    ports: ["8080:8080", "8082:8082"]
    volumes:
      - "../../tools/capture:/scripts:ro"
      - "../../test/golden/raw:/captures"

volumes:
  pgdata:
  miniodata:
```

## Week-by-Week Schedule (8 weeks baseline, 10 with slack)

### Week 1 — Bootstrap
- `go.mod`, `Makefile`, `.golangci.yml`, GitHub Actions CI
- ADR-0001 (tech stack), ADR-0002 (greenfield decision), ADR-0003 (WASM plugins)
- `cmd/ncgo/main.go` — boots, serves `/status.php` returning hardcoded JSON
- `internal/config/` — koanf loader, schema validation
- Dockerfile (multi-stage, distroless final)
- `docker-compose.dev.yml` standing up PG/MySQL/Redis/MinIO

### Week 2 — DB & Migrations
- `internal/db/` — DB interface + pgx, mysql, sqlite drivers
- `migrations/0001_init.sql` (Postgres dialect) + MySQL & SQLite variants
- Migration runner integrated into `ncgo` startup and `ncgo-cli migrate`
- Stand up reference Nextcloud + mitmproxy
- Begin enumerating Nextcloud's WebDAV custom property surface from PHP source

### Week 3 — Capture Sprint Part 1
- mitmproxy capture scripts, scenario runbook
- Capture: desktop login, capability fetch, initial sync (10 files), incremental sync
- 20+ raw HAR files committed
- `tools/golden-gen/` v0 → typed golden cases for ~10 scenarios

### Week 4 — HTTP Layer & OCS
- `internal/httpx/` — chi router, middleware (logging, recovery, request ID, auth)
- `internal/ocs/` — OCS envelope encoding (XML + JSON), capability registry
- `cloud/capabilities` returns valid envelope (still mostly empty payload)
- `cloud/user` returns mock user
- `cmd/ncgo-captest/` v0 — replays first 10 golden cases

### Week 5 — Capture Sprint Part 2
- Capture: iOS Files.app, macOS Finder, Android Nextcloud app
- Capture: chunked upload, share creation, public link visit, CalDAV (read), CardDAV (read)
- 50+ raw HAR files total committed under `test/golden/raw/`
- Convert all to typed golden cases

### Week 6 — Auth Foundations & Cache
- `internal/auth/` — Identity, Session, AppPassword types; Argon2id password hashing
- `internal/cache/` — tiered cache (ristretto + go-redis)
- Login flow v2 step 1 (poll endpoint) — returns valid token shape
- (Login flow completion is Phase 1)

### Week 7 — WASM Plugin Stub
- `internal/plugin/` — wazero runtime, manifest TOML parser
- ABI v0: `ncgo.log` only
- `pkg/pluginsdk/` — Go bindings for `Info`/`Warn`/`Error`
- `examples/hello-plugin/` — TinyGo plugin that logs on `ncgo_on_install`
- Integration test: load plugin → verify log line

### Week 8 — Hardening, CI, Exit Criteria
- CI matrix: Linux/macOS × Go 1.22/1.23 × {Postgres, MySQL, SQLite}
- `golangci-lint` clean; `go vet` clean; `staticcheck` clean
- Coverage instrumentation (target: ≥60% in Phase 0; rises in later phases)
- Documentation: this directory finalized; README updated; CONTRIBUTING.md drafted
- Phase 0 exit review

## Exit Criteria (verified before Phase 1 starts)

- [ ] `ncgo` binary boots with valid config
- [ ] `GET /status.php` returns valid Nextcloud-shape JSON
- [ ] `POST /ocs/v2.php/cloud/capabilities` returns valid OCS envelope
- [ ] Migrations run cleanly on PostgreSQL 16, MySQL 8, SQLite (modernc)
- [ ] ≥50 captured golden cases archived; ≥10 replayable through `ncgo-captest`
- [ ] WASM hello-world plugin loads and logs to host
- [ ] CI green on Linux/macOS × Go 1.22/1.23
- [ ] `golangci-lint`, `go vet`, `staticcheck` all clean
- [ ] Test coverage ≥60%
- [ ] All ADRs accepted and committed
- [ ] Phase 1 plan drafted and reviewed

## Risks Specific to Phase 0

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Mobile clients use TLS pinning, blocking mitmproxy | High | High | Fall back to Charles, sslsplit, or decompiled APK with pinning removed (private testing only) |
| WebDAV custom property catalog incomplete | Medium | High | Cross-reference PHP source + captured PROPFIND responses |
| `x/net/webdav` reveals deeper limitations than expected | Low | Medium | Already planning to fork; treat as "rewrite incrementally" not "patch" |
| Capture sprint slips, downstream phases starved of test data | Medium | High | Allocate dedicated engineer for Weeks 3 & 5; scenarios prioritized by phase need |
| Plugin SDK API changes during Phase 0 break example plugin | Low | Low | Pin SDK version; example plugin in same repo, regenerated on SDK changes |

## Change Log

- **2026-04-29** — Initial blueprint committed.
