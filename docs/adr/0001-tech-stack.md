# ADR-0001: Tech Stack Selection

- **Status**: 🟢 Accepted
- **Date**: 2026-04-29
- **Deciders**: Project lead

## Context

`nextcloud-go` is a ground-up Go rewrite of Nextcloud Server. Tech stack choices made
now will be hard to change later — they shape every subsequent design decision. We need
explicit, defensible choices for:

- HTTP routing
- Dependency injection
- Database drivers (Postgres, MySQL, SQLite) and query construction
- Migrations
- Caching (in-memory + distributed)
- WebDAV core
- Object storage clients
- WASM runtime (for plugin system)
- Logging, metrics, tracing
- Configuration loading
- CLI framework
- Testing

Cross-cutting constraints:

1. **No CGO** unless absolutely required. Single static binary, easy cross-compilation.
2. **Pure Go** preferred where multiple options exist.
3. **Maintenance health** matters — we're committing to dependencies for years.
4. **Idiomatic stdlib usage** preferred over heavy frameworks.

## Decision

| Concern | Choice |
|---|---|
| HTTP router | `github.com/go-chi/chi/v5` |
| DI | Hand-rolled constructor injection in `internal/app/` |
| Postgres driver | `github.com/jackc/pgx/v5` |
| MySQL driver | `github.com/go-sql-driver/mysql` |
| SQLite driver | `modernc.org/sqlite` (pure Go) |
| Query builder | `github.com/Masterminds/squirrel` |
| Migrations | `github.com/golang-migrate/migrate/v4` |
| Redis client | `github.com/redis/go-redis/v9` |
| In-memory cache | `github.com/dgraph-io/ristretto` |
| WebDAV core | Fork `golang.org/x/net/webdav` |
| S3 client | `github.com/aws/aws-sdk-go-v2` + `github.com/minio/minio-go/v7` |
| WASM runtime | `github.com/tetratelabs/wazero` (pure Go, no CGO) |
| Logging | `log/slog` (stdlib) |
| Metrics | `github.com/prometheus/client_golang` |
| Tracing | `go.opentelemetry.io/otel` |
| Config | `github.com/knadh/koanf/v2` |
| CLI framework | `github.com/spf13/cobra` |
| Testing | `github.com/stretchr/testify` + `github.com/testcontainers/testcontainers-go` |

## Alternatives Considered

### HTTP router: chi vs. gin/echo/fiber/gorilla
- chi: stdlib `http.Handler` compatible, middleware chains, sub-routers, low surface area, very stable
- gin/echo: their own context type, away from stdlib idioms
- fiber: built on fasthttp, NOT stdlib-compatible (problem for WebDAV which uses `http.ResponseWriter`)
- gorilla/mux: archived/maintenance mode

**Picked chi** for stdlib compatibility and longevity.

### DI: hand-rolled vs. wire/fx
- wire: code generation, compile-time, but feels heavy for our scale (~30 components)
- fx: runtime, reflection-based, harder to debug
- Hand-rolled: explicit `func New(deps...) *App`, one file, no magic

**Picked hand-rolled.** The whole wiring fits in <500 lines and is greppable.

### SQLite: modernc vs. mattn vs. crawshaw
- mattn/go-sqlite3: most popular, but **CGO** — kills our static-binary goal
- crawshaw/sqlite: also CGO
- modernc.org/sqlite: SQLite transpiled to pure Go. Slower than CGO but acceptable
  for the embedded/test scenarios where we use SQLite

**Picked modernc** — non-negotiable on the no-CGO rule.

### Query builder: squirrel vs. ORM (gorm/ent/sqlc)
- gorm: full ORM, magic, auto-migration; we want explicit migrations and explicit SQL
- ent: code generation from schema, opinionated; doesn't fit migrating from PHP's
  varied data access patterns
- sqlc: SQL-first code gen; great but rigid (every query needs codegen pass)
- squirrel: programmatic SQL builder, no magic, plays well with `database/sql`

**Picked squirrel.** Nextcloud's data access is too varied for an ORM; squirrel keeps
SQL inspectable.

### WASM runtime: wazero vs. wasmer-go vs. wasmtime-go
- wasmer-go, wasmtime-go: both **CGO**
- wazero: pure Go, mature (Tetrate-backed), good performance, supports our needs
  (host functions, fuel metering, memory limits, custom imports)

**Picked wazero** — only pure-Go option meeting our requirements.

### WebDAV: fork x/net/webdav vs. SabreDAV port vs. from scratch
- SabreDAV port: massive PHP codebase; porting verbatim defeats the rewrite purpose
- From scratch: too much WebDAV/RFC complexity to redo (locking, copy/move semantics)
- Fork x/net/webdav: solid base, but missing Nextcloud's custom property surface, ETag
  algorithm, chunked upload, and many WebDAV extensions

**Picked: fork x/net/webdav.** Treat the fork as a starting point and rewrite incrementally.
Do not attempt to upstream Nextcloud-specific extensions.

### Logging: slog vs. logrus/zap/zerolog
- logrus: maintenance mode
- zap, zerolog: faster than slog but external dep; slog is now the stdlib answer
- slog: stdlib, structured, handler-pluggable

**Picked slog.** Stdlib wins ties.

### Config: koanf vs. viper
- viper: huge dep tree, magic, opinionated
- koanf: modular, you compose only what you need (YAML loader, env var overrides, etc.)

**Picked koanf.** Smaller surface, no surprises.

## Consequences

### Positive
- Single static binary on every supported platform
- Dependency tree stays small and inspectable
- Most choices are stdlib-adjacent → low long-term maintenance risk
- WASM plugin system can ship without CGO

### Negative
- modernc SQLite ~3-5× slower than CGO mattn driver (acceptable for our use case;
  not used as primary production DB)
- Forking x/net/webdav means we own all WebDAV bugs from now on
- Hand-rolled DI requires discipline as the codebase grows

### Neutral / follow-ups
- Re-evaluate slog handlers once Go 1.24+ ecosystem matures
- Re-evaluate WASM component model (WASI Preview 2) for ABI v2

## References

- [chi](https://github.com/go-chi/chi)
- [pgx](https://github.com/jackc/pgx)
- [modernc sqlite](https://gitlab.com/cznic/sqlite)
- [squirrel](https://github.com/Masterminds/squirrel)
- [wazero](https://wazero.io)
- [koanf](https://github.com/knadh/koanf)
