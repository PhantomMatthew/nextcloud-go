# WASM Plugin ABI Specification

**Status**: 🟡 Draft (Phase 0 deliverable, full implementation in Phase 4)
**ABI Version**: `ncgo-abi/1`
**Runtime**: [`github.com/tetratelabs/wazero`](https://wazero.io) (pure Go, no CGO)
**Last updated**: 2026-04-29

## 1. Design Goals

1. **Sandboxed by default** — plugins cannot access host filesystem, network, env, or
   other plugins' state without explicit capability grants
2. **Language-agnostic** — Go, Rust, AssemblyScript, Zig, TinyGo can all target the ABI
3. **Stable across host versions** — versioned ABI; host implements N and N-1
4. **Capability-based security** — plugins declare needs in manifest; admin grants per install
5. **Performance acceptable** — sub-millisecond overhead per host call for hot paths
   (DB query, cache get)
6. **Streaming-friendly** — large file uploads/downloads must not require buffering whole
   payload in WASM linear memory
7. **Debuggable** — every host call traced with plugin ID, function, duration, error

## 2. Non-Goals (v1)

- Hot-reload of plugins (future)
- Plugin-to-plugin direct calls (always go through host event bus)
- Native code execution (WASM only; no plugin-supplied `.so`/`.dll`)
- GUI plugins for the web frontend (separate concern)

## 3. Plugin Lifecycle

```
   ┌─────────────┐  install   ┌──────────────┐
   │ .ncplugin   │──────────▶│  Plugin       │
   │ archive     │            │  Registry     │
   └─────────────┘            └──────┬────────┘
                                     │ resolve manifest
                                     │ check capabilities
                                     │ admin approval
                                     ▼
                              ┌──────────────┐
                              │ Loaded       │  wazero.CompileModule
                              │ (compiled)   │  (cached on disk)
                              └──────┬───────┘
                                     │ on-demand instantiate
                                     ▼
                              ┌──────────────┐
                              │ Instantiated │  one instance per request OR
                              │              │  pooled (per manifest hint)
                              └──────┬───────┘
                                     │ host invokes exported fn
                                     ▼
                              ┌──────────────┐
                              │ Running      │  plugin executes,
                              │              │  may call host functions
                              └──────┬───────┘
                                     │ return / trap / timeout
                                     ▼
                              ┌──────────────┐
                              │ Released     │  instance closed,
                              │              │  memory freed
                              └──────────────┘
```

**Instance models** (declared in manifest):

- `per_request` — fresh instance per HTTP request (safest, slowest; default)
- `pooled` — N pre-warmed instances; request gets one from pool, returned after
- `singleton` — one instance, mutex-serialized (only for stateful background plugins)

## 4. Plugin Manifest (`plugin.toml`)

```toml
[plugin]
id              = "com.example.calendar-sync"
name            = "Calendar Sync"
version         = "1.2.0"
abi             = "ncgo-abi/1"
description     = "Two-way sync with external CalDAV servers"
author          = "Example Inc."
homepage        = "https://example.com/plugins/calendar-sync"
license         = "MIT"

[runtime]
instance_model  = "pooled"          # per_request | pooled | singleton
pool_size       = 4                  # only for pooled
memory_limit_mb = 32                 # max linear memory
cpu_timeout_ms  = 5000               # per host call to plugin
fuel_per_call   = 100_000_000        # wazero metering budget

[capabilities]
db.read         = ["calendar_*"]     # table name globs
db.write        = ["calendar_sync_state"]
storage.read    = ["user"]           # user files vs system
storage.write   = []
http.outbound   = ["*.icloud.com:443", "caldav.google.com:443"]
events.publish  = ["calendar.synced"]
events.subscribe = ["user.deleted", "calendar.event.created"]
jobs.register   = true
routes.register = ["/apps/calendar-sync/*"]
ocs.register    = ["/apps/calendar-sync/api/v1/*"]
webdav.props    = ["sync-token"]
config.read     = ["calendar_sync.*"]
config.write    = ["calendar_sync.*"]

[entry_points]
module          = "calendar_sync.wasm"
on_install      = "ncgo_on_install"
on_uninstall    = "ncgo_on_uninstall"
on_request      = "ncgo_on_request"
on_job          = "ncgo_on_job"
on_event        = "ncgo_on_event"

[settings]
schema_file     = "settings_schema.json"
```

**Archive format** `.ncplugin` (zip):

```
calendar-sync-1.2.0.ncplugin
├── plugin.toml
├── calendar_sync.wasm
├── settings_schema.json
├── i18n/
│   ├── en.json
│   └── de.json
└── README.md
```

## 5. Capability Model

Capabilities are declarative permissions the plugin **requests**; admins **grant** at
install/upgrade.

| Capability | Scope | Grant Implications |
|---|---|---|
| `db.read = [globs]` | Read tables matching globs | Plugin sees raw rows; no row-level security |
| `db.write = [globs]` | Write tables matching globs | Migrations for plugin tables run at install |
| `storage.read = ["user"\|"system"]` | Read user-scoped or system-scoped files | User scope = current request's user only |
| `storage.write = ...` | Write files | Subject to user quota |
| `http.outbound = [host:port]` | Outbound HTTP allowlist | Each request validated against list |
| `events.publish = [topics]` | Emit events | Topics namespaced; cannot publish to `core.*` |
| `events.subscribe = [topics]` | Receive events | Includes wildcards |
| `jobs.register` | Register background jobs | Jobs run with plugin's caps |
| `routes.register = [paths]` | Mount HTTP routes | Paths must start with `/apps/<plugin-id>/` |
| `ocs.register = [paths]` | Mount OCS endpoints | Same prefix rule |
| `webdav.props = [names]` | Provide WebDAV custom properties | Name must be plugin-namespaced |
| `config.read/write = [keys]` | Plugin config namespace | Globs against `module_config` table |

**Default deny.** Any host call requiring a capability not granted → trap with
`ErrPermissionDenied`.

## 6. ABI: Host Functions Exposed to Plugins

All host functions live under WASM module name `ncgo`. Calling convention follows
wasi-style: pass pointers + lengths to linear memory; return error code as `i32`,
output written via host-allocated buffers or callback.

### 6.1 Memory Convention

- Plugin exports `ncgo_alloc(size: i32) -> i32` and `ncgo_free(ptr: i32, size: i32)`
- Host uses these to write data into plugin's linear memory
- All strings are UTF-8, length-prefixed (no null terminators)
- All structured data is **MessagePack** encoded (smaller than JSON, faster than
  protobuf for dynamic schemas)

### 6.2 Error Codes (i32)

```
0   = OK
-1  = ErrInternal
-2  = ErrInvalidArgument
-3  = ErrPermissionDenied
-4  = ErrNotFound
-5  = ErrAlreadyExists
-6  = ErrTimeout
-7  = ErrCanceled
-8  = ErrQuotaExceeded
-9  = ErrUnsupported
-10 = ErrConflict
-11 = ErrTooLarge
-12 = ErrUnavailable
```

### 6.3 Function Catalog

#### Logging (always granted)

```
ncgo.log(level: i32, msg_ptr: i32, msg_len: i32) -> i32
  level: 0=debug 1=info 2=warn 3=error
```

#### Context

```
ncgo.ctx_user_id(out_ptr: i32, out_max: i32) -> i32       // bytes written or err
ncgo.ctx_request_id(out_ptr: i32, out_max: i32) -> i32
ncgo.ctx_locale(out_ptr: i32, out_max: i32) -> i32
ncgo.ctx_deadline_unix_ms() -> i64                        // 0 if no deadline
```

#### Configuration

```
ncgo.config_get(key_ptr, key_len, out_ptr, out_max) -> i32
ncgo.config_set(key_ptr, key_len, val_ptr, val_len) -> i32
  Capability: config.read / config.write
  Keys auto-namespaced under "plugin.<plugin_id>."
```

#### Database (handle-based)

```
ncgo.db_query(sql_ptr, sql_len, args_ptr, args_len) -> i64
  Returns: high32 = error_code, low32 = result_handle (if OK)
  args is MessagePack array of values
  Capability: db.read

ncgo.db_exec(sql_ptr, sql_len, args_ptr, args_len, out_rowsaffected: i32) -> i32
  Capability: db.write

ncgo.db_rows_next(handle, out_ptr, out_max) -> i32
ncgo.db_rows_close(handle: i32) -> i32

ncgo.db_tx_begin() -> i64
ncgo.db_tx_commit(tx_handle: i32) -> i32
ncgo.db_tx_rollback(tx_handle: i32) -> i32
ncgo.db_tx_query(tx_handle, sql_ptr, sql_len, args_ptr, args_len) -> i64
ncgo.db_tx_exec(tx_handle, sql_ptr, sql_len, args_ptr, args_len, out_rows) -> i32
```

**SQL safety**: Host parses SQL with a dialect-aware parser (pg_query_go for Postgres,
Vitess parser for MySQL, custom shim for SQLite) and rejects any statement touching
tables outside the plugin's `db.*` capability globs. No raw `EXEC`, no DDL outside
install/uninstall hooks.

#### Cache

```
ncgo.cache_get(key_ptr, key_len, out_ptr, out_max) -> i32
ncgo.cache_set(key_ptr, key_len, val_ptr, val_len, ttl_seconds: i32) -> i32
ncgo.cache_delete(key_ptr, key_len) -> i32
ncgo.cache_increment(key_ptr, key_len, delta: i64, out_new: i32) -> i32
  Keys auto-namespaced "plugin:<plugin_id>:"
  Always granted (sandboxed namespace).
```

#### Storage (streaming)

```
ncgo.storage_stat(path_ptr, path_len, out_ptr, out_max) -> i32
  out = MessagePack FileInfo

ncgo.storage_open(path_ptr, path_len) -> i64
ncgo.storage_create(path_ptr, path_len, size: i64) -> i64
ncgo.storage_stream_read(handle, buf_ptr, buf_max) -> i32
ncgo.storage_stream_write(handle, buf_ptr, buf_len) -> i32
ncgo.storage_stream_close(handle: i32) -> i32

ncgo.storage_delete(path_ptr, path_len) -> i32
ncgo.storage_list(path_ptr, path_len, out_ptr, out_max) -> i32
ncgo.storage_rename(src_ptr, src_len, dst_ptr, dst_len) -> i32

  Capability: storage.read / storage.write
  All paths relative to user's root (or system root if "system" granted)
```

#### HTTP outbound

```
ncgo.http_request(req_ptr, req_len) -> i64
  req = MessagePack { method, url, headers, body_bytes, timeout_ms }
  Returns: high32 err, low32 response_handle

ncgo.http_response_status(handle: i32) -> i32
ncgo.http_response_header(handle, name_ptr, name_len, out_ptr, out_max) -> i32
ncgo.http_response_body_read(handle, buf_ptr, buf_max) -> i32
ncgo.http_response_close(handle: i32) -> i32

  Capability: http.outbound
```

#### Events

```
ncgo.event_publish(topic_ptr, topic_len, payload_ptr, payload_len) -> i32
  Capability: events.publish

// Subscription is registered via manifest + on_event entry point;
// host calls plugin's exported ncgo_on_event(topic, payload) when events fire.
```

#### Jobs

```
ncgo.job_enqueue(name_ptr, name_len, payload_ptr, payload_len, run_at_unix_ms: i64) -> i32
  Capability: jobs.register
  Job names auto-namespaced.
```

#### Route registration (called only during on_install)

```
ncgo.route_register(method_ptr, method_len, path_ptr, path_len,
                    handler_name_ptr, handler_name_len) -> i32
  Capability: routes.register
  Path must start with /apps/<plugin_id>/

ncgo.ocs_register(method_ptr, method_len, path_ptr, path_len,
                  handler_name_ptr, handler_name_len) -> i32
  Capability: ocs.register
```

#### WebDAV property registration (on_install)

```
ncgo.webdav_register_prop(name_ptr, name_len,
                          getter_name_ptr, getter_name_len,
                          setter_name_ptr, setter_name_len) -> i32
  Capability: webdav.props
  Setter may be empty for read-only props
```

#### Cryptography (always granted)

```
ncgo.crypto_random(out_ptr, out_len) -> i32
ncgo.crypto_hash(algo: i32, in_ptr, in_len, out_ptr, out_max) -> i32
  algo: 0=sha256 1=sha512 2=blake2b
ncgo.crypto_hmac(algo, key_ptr, key_len, msg_ptr, msg_len, out_ptr, out_max) -> i32
```

## 7. ABI: Functions the Plugin Exports to the Host

```
ncgo_abi_version() -> i32                                 // returns 1
ncgo_alloc(size: i32) -> i32
ncgo_free(ptr: i32, size: i32)

// Lifecycle
ncgo_on_install() -> i32                                  // run migrations, register routes
ncgo_on_uninstall() -> i32                                // cleanup
ncgo_on_upgrade(from_version_ptr, from_version_len) -> i32

// HTTP request handling
ncgo_on_request(req_ptr: i32, req_len: i32) -> i64
  req = MessagePack { method, path, headers, query, body_handle }
  returns: high32 err, low32 response_handle (plugin allocs)

ncgo_response_status(handle: i32) -> i32
ncgo_response_header_count(handle: i32) -> i32
ncgo_response_header_at(handle, idx, out_ptr, out_max) -> i32
ncgo_response_body_read(handle, buf_ptr, buf_max) -> i32
ncgo_response_close(handle: i32) -> i32

// Background job execution
ncgo_on_job(name_ptr, name_len, payload_ptr, payload_len) -> i32

// Event reception
ncgo_on_event(topic_ptr, topic_len, payload_ptr, payload_len) -> i32

// WebDAV property getter/setter (names match those given to webdav_register_prop)
<plugin-defined>(resource_path_ptr, len) -> i64           // returns value MessagePack or err
```

## 8. Sandboxing & Resource Limits

Enforced by wazero configuration per instance:

| Limit | Default | Configurable in Manifest |
|---|---|---|
| Linear memory max | 32 MiB | `runtime.memory_limit_mb` (host-capped at 256) |
| CPU fuel per host call | 100M units | `runtime.fuel_per_call` |
| Wall-clock timeout per host call | 5 s | `runtime.cpu_timeout_ms` (host-capped at 30s) |
| Max open stream handles | 64 | not configurable |
| Max open DB rows handles | 16 | not configurable |
| Max open HTTP response handles | 16 | not configurable |
| Filesystem access (WASI) | **none** | not configurable |
| Network sockets (WASI) | **none** | not configurable (use `http_request` only) |
| Env vars / args (WASI) | **empty** | not configurable |
| Random source | host-provided via `crypto_random` | always granted |
| Clock | monotonic + wall via host functions | always granted |

**WASI is not exposed.** Plugins cannot use `wasi_snapshot_preview1` to bypass the
ABI. wazero is configured with no WASI module attached.

**Trap handling**: Any trap (memory OOB, division by zero, fuel exhaustion, timeout)
→ instance destroyed, error logged with plugin ID + stack trace, request fails with
HTTP 500 (or job marked failed).

## 9. Versioning & Compatibility

- ABI string: `ncgo-abi/<MAJOR>` — breaking changes bump major
- Host implements current major + previous major during deprecation window (1 release)
- New host functions added within a major are OK (plugins linking against older ABI
  continue to work; missing functions trap if called)
- Removed functions = major bump
- Plugin manifest's `[plugin].abi` checked at load; mismatch → load refused

Plugin SDK convention (`pkg/pluginsdk/`):

```go
//go:build wasm

package pluginsdk

const ABIVersion = 1

//go:wasmimport ncgo log
func hostLog(level int32, msgPtr, msgLen uint32) int32

func Info(msg string) {
    p, l := stringPtr(msg)
    hostLog(1, p, l)
}
```

## 10. Example Plugin Walkthrough — File Tagger

**Spec**: When a user uploads a file ending in `.invoice.pdf`, automatically tag it
with `auto:invoice`. Provides an OCS endpoint to list all auto-tagged files.

### Manifest

```toml
[plugin]
id      = "com.example.file-tagger"
name    = "Auto File Tagger"
version = "0.1.0"
abi     = "ncgo-abi/1"

[runtime]
instance_model = "pooled"
pool_size      = 2

[capabilities]
db.read         = ["file_tags"]
db.write        = ["file_tags"]
events.subscribe = ["files.uploaded"]
ocs.register    = ["/apps/file-tagger/api/v1/list"]

[entry_points]
module       = "tagger.wasm"
on_install   = "ncgo_on_install"
on_event     = "ncgo_on_event"
on_request   = "ncgo_on_request"
```

### Plugin Code (Go, compiled with TinyGo to WASM)

```go
//go:build wasm
package main

import (
    "strings"
    "github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

//export ncgo_on_install
func onInstall() int32 {
    pluginsdk.DBExec(`
        CREATE TABLE IF NOT EXISTS file_tags (
            file_path TEXT NOT NULL,
            tag       TEXT NOT NULL,
            PRIMARY KEY (file_path, tag)
        )`)
    pluginsdk.OCSRegister("GET", "/apps/file-tagger/api/v1/list", "list_handler")
    return 0
}

//export ncgo_on_event
func onEvent(topic, payload string) int32 {
    if topic != "files.uploaded" {
        return 0
    }
    var ev struct{ Path string }
    pluginsdk.Unmarshal(payload, &ev)

    if strings.HasSuffix(ev.Path, ".invoice.pdf") {
        pluginsdk.DBExec(
            "INSERT INTO file_tags(file_path, tag) VALUES (?, ?) ON CONFLICT DO NOTHING",
            ev.Path, "auto:invoice",
        )
        pluginsdk.Info("tagged " + ev.Path)
    }
    return 0
}

//export ncgo_on_request
func onRequest(req pluginsdk.Request) pluginsdk.Response {
    rows, _ := pluginsdk.DBQuery(
        "SELECT file_path FROM file_tags WHERE tag = ?", "auto:invoice",
    )
    defer rows.Close()
    var paths []string
    for rows.Next() {
        var p string
        rows.Scan(&p)
        paths = append(paths, p)
    }
    return pluginsdk.JSON(200, map[string]any{"files": paths})
}

func main() {} // required for TinyGo
```

### Host's view of the request flow

```
1. User uploads /docs/2026-04-Acme.invoice.pdf via WebDAV PUT
2. files module commits the upload, emits event "files.uploaded" {Path: "/docs/..."}
3. EventBus matches subscription com.example.file-tagger -> files.uploaded
4. Host plugin manager:
   - acquires pooled instance
   - sets ctx (user_id, request_id, deadline)
   - calls instance.ncgo_on_event(topic="files.uploaded", payload=mp(...))
5. Plugin code matches suffix, calls ncgo.db_exec
   - host validates: capability db.write covers "file_tags" ✓
   - host parses SQL, confirms only file_tags table touched ✓
   - host executes via pgx, returns rows_affected
6. Plugin returns 0 (OK)
7. Host returns instance to pool, releases handles
8. Total observed overhead: ~0.3ms (instantiation amortized via pool)
```

## 11. Performance Budget

Measured targets (Phase 4 acceptance):

| Operation | Budget | Notes |
|---|---|---|
| Pooled instance acquire | < 10 µs | Pre-warmed |
| Per-request instance instantiate | < 2 ms | Acceptable for cold paths |
| `ncgo.log` host call | < 5 µs | Native logger |
| `ncgo.cache_get` (L1 hit) | < 50 µs | ristretto + msgpack |
| `ncgo.db_query` (simple SELECT) | < 1 ms | Dominated by SQL parse + DB roundtrip |
| `ncgo.storage_stream_read` (64 KiB) | < 100 µs | Buffer copy + memory write |
| Plugin event delivery (in-process) | < 500 µs | Per subscriber |

Microbenchmarks live in `internal/plugin/bench_test.go`, gated in CI.

## 12. Observability

Each host call emits:

- Prometheus counter `ncgo_plugin_host_calls_total{plugin, function, result}`
- Prometheus histogram `ncgo_plugin_host_call_duration_seconds{plugin, function}`
- OTel span (sampled) `plugin.host_call` with attrs: plugin.id, abi.version, function, error

Per-plugin admin dashboard (Phase 4 UI):

- Request count, error rate, p50/p95/p99 latency
- Memory high-water mark
- Capability denial events (security signal)

## 13. Security Review Checklist

- [ ] No `wasi_snapshot_preview1` exposed
- [ ] All SQL parsed and table-allowlisted before execution
- [ ] All filesystem paths normalized + jailed to user/system root before any storage op
- [ ] All HTTP outbound URLs resolved + IP-checked against host allowlist (block private
      IP ranges unless explicitly granted)
- [ ] Plugin module signature verified at install (Phase 4: ed25519 signature in `.ncplugin`)
- [ ] Resource limits enforced via wazero config, not soft checks
- [ ] Trap on integer overflow in handle arithmetic
- [ ] Handle tables per-instance; cross-instance handle reuse rejected
- [ ] Memory writes from host bounds-checked against plugin's reported memory size
- [ ] Capability changes on upgrade require admin re-approval

## 14. Open Questions for Phase 4 Implementation

1. **SQL parser choice for safety check**: `pg_query_go` is Postgres-only. For
   MySQL/SQLite plugins need a dialect-aware parser. Candidate: `vitess` SQL parser
   (handles MySQL well) + custom shim for SQLite.
2. **Plugin signing infrastructure**: Self-hosted CA vs. publisher-managed keys vs.
   TUF-style trust delegation?
3. **Multi-tenant plugin install** (per-user enable/disable vs. global only)?
4. **Plugin marketplace / discovery** out of scope for v1; document as future work.
5. **WASM threads**: wazero supports them but most toolchains (TinyGo) don't emit
   them; defer.
6. **Component model** (WASI Preview 2 / WIT): Adopt now (cleaner ABI but ecosystem
   immature) or stick with raw WASM imports? Recommendation: raw imports for v1,
   evaluate component model in v2.

## 15. Phase 0 Deliverable for the ABI

In Phase 0, ship only the **stub host**:

- `internal/plugin/host.go` — wazero runtime initialization, no-op modules
- `internal/plugin/abi.go` — `ncgo.log` only (proves the host call mechanism works)
- `internal/plugin/manifest.go` — TOML parser for `plugin.toml`
- `pkg/pluginsdk/` — Go bindings for `Info`/`Warn`/`Error` only
- `examples/hello-plugin/` — TinyGo plugin that logs "hello from wasm" on `ncgo_on_install`
- Integration test: load → install → verify log line → unload

Full ABI implementation is the bulk of Phase 4.

## Change Log

- **2026-04-29** — Initial spec committed.
