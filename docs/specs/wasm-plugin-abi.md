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
pool_size       = 4                  # only for pooled, 1..32
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
| `http.outbound_allow_private = true|false` | Reach loopback/private/link-local/unspecified target IPs | Off by default; without it the dial-time IP check (SSRF guard) refuses internal targets even when the hostname allowlist matches |
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
ncgo.storage_mkdir(path_ptr, path_len) -> i32
  Creates a single directory; no implicit parents. Existing target -> -5,
  missing parent -> -4. Directories carry no bytes: no quota applies
  (ADR-0061/0067).

  Capability: storage.read / storage.write
  All paths relative to user's root (or system root if "system" granted)
```

#### HTTP outbound

```
ncgo.http_request(req_ptr, req_len) -> i64
  req = MessagePack { method, url, headers, body_bytes, body_handle, timeout_ms }
  body_bytes and body_handle are mutually exclusive; a map carrying both is
  refused with -2. body_handle references a sealed spool from
  http_request_body_create (below) and is consumed and destroyed by the call.
  Returns: high32 err, low32 response_handle

ncgo.http_response_status(handle: i32) -> i32
ncgo.http_response_header(handle, name_ptr, name_len, out_ptr, out_max) -> i32
ncgo.http_response_body_read(handle, buf_ptr, buf_max) -> i32
ncgo.http_response_close(handle: i32) -> i32

  Capability: http.outbound
```

Outbound targets are also IP-checked at dial time: the resolved address is refused
when it is loopback, private (RFC1918/ULA), link-local, or unspecified, unless the
plugin additionally holds `http.outbound_allow_private = true` (SSRF guard,
ADR-0057).

Each `http_request` call — redirect chain included — draws one token from a
per-plugin-id bucket (`plugin.http_rate_per_minute`, default 120, burst 30);
exhaustion fails the call with -8 (`ErrQuotaExceeded`). Response bodies are capped
at `plugin.max_http_response_mb` (default 32 MiB): the `http_response_body_read`
that would push delivered bytes past the cap fails loudly with -11 (`ErrTooLarge`)
instead of silently truncating, later reads keep failing, and status/header reads
are unaffected (ADR-0059).

#### Outbound request body (streaming)

```
ncgo.http_request_body_create() -> i64
ncgo.http_request_body_write(handle, buf_ptr, buf_len) -> i32
ncgo.http_request_body_close(handle: i32) -> i32
```

These stage a request body larger than the 1 MiB inline `body_bytes` cap in a
temp-file spool, referenced from `http_request`'s `body_handle` (ADR-0066).
`create` returns high32 err / low32 handle and is gated on `http.outbound`
like `http_request` itself (the per-target allowlist still applies at request
time); the handle shares the §8 stream budget (64 per plugin, shared across
instances). `write`
appends one chunk and returns the byte count; `buf_len` above the host
payload cap (1 MiB) fails with -11 (`ErrTooLarge`), and the write that would
push the running total past the host spool cap (default 1 GiB, the same
`HostConfig.MaxSpoolBytes` knob as the storage write spools) fails with -11.
`close` seals the spool: writes and repeated closes after the seal answer -2
(`ErrInvalidArgument`). `http_request` requires a sealed handle — an unsealed
one is -2 and stays usable, a missing id -4, an id of another handle type -2
— and consumption is destructive: the request goes out with a known
`ContentLength` (`GetBody` reopens the spool so 307/308 redirects can replay
it), and the spool file is deleted when the call ends, success or failure. A
spool never consumed is deleted by the handle-table cleanup when the instance
is released. Allowlist, egress IP guard, rate limit, and the response byte
cap apply to the streamed path unchanged; allowlist and rate-limit denials
are checked before consumption, so a refused request leaves the sealed spool
intact for a retry.

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

#### Route request body (streaming, opt-in)

```
ncgo.request_body_read(handle, buf_ptr, buf_max) -> i32
ncgo.request_body_close(handle: i32) -> i32
```

These pull the inbound route request body behind the §7 `body_handle` and
mirror `http_response_body_read`/`http_response_close` semantics:
`request_body_read` returns the byte count (>0), 0 at EOF, or a negative
error code; `buf_max` above the host payload cap (1 MiB) fails with -11
(`ErrTooLarge`). `request_body_close` releases the handle early; reading a
closed handle answers -4 (`ErrNotFound`), and a handle of any other type
answers -2 (`ErrInvalidArgument`). Only plugins with
`runtime.request_body_stream = true` receive a `body_handle` (§7); the body
is the plugin's own inbound request, so no capability gates these functions.
The handle shares the §8 stream budget (64 per plugin, shared across
instances) and, if never
closed, is released by handle-table cleanup when the instance is released —
the body itself remains owned by the host's HTTP server throughout.

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

The `body_handle` request map is **opt-in**: it is sent only to plugins with
`runtime.request_body_stream = true` in the manifest, and the body is pulled
via `ncgo.request_body_read`/`ncgo.request_body_close` (§6.3). Without the
opt-in the map carries the body inline instead —
`{ method, path, headers, query, body_bytes }` with `body_bytes` capped at
1 MiB (the host answers 413 above without invoking the plugin) — which keeps
pre-existing guests byte-compatible. Once `ncgo_on_request` has returned, the
response streaming phase begins and the guest must not read the body any
longer.

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

**Handle budgets are per plugin, shared across instances** (ADR-0068): the
three open-handle rows above — 64 stream, 16 DB rows, 16 HTTP response —
bound one plugin's handles aggregated across all of its live instances, not
each instance separately. The pooled and per_request instance models run
several instances concurrently, and open rows handles pin `database/sql`
pool connections, so a per-instance reading would let a plugin's open-handle
footprint scale with its instance count; a pooled plugin's whole fleet can
hold at most 16 open rows handles at once. Handle *ids* stay
instance-scoped — an id handed to one instance is meaningless to another —
and exhaustion still answers `ErrUnavailable` (-12) from the same host
calls, so well-behaved guests need no change (a guest only observes a
refusal its per-instance code path already had to handle).

**WASI is not exposed.** Plugins cannot use `wasi_snapshot_preview1` to bypass the
ABI. wazero is configured with no WASI module attached.

**Trap handling**: Any trap (memory OOB, division by zero, fuel exhaustion, timeout)
→ instance destroyed, error logged with plugin ID + stack trace, request fails with
HTTP 502 (or job marked failed).

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

Storage transfers additionally emit (ADR-0067):

- Prometheus counter `ncgo_plugin_storage_bytes_total{plugin, op, scope}` with
  `op ∈ read|write`, `scope ∈ user|system`, counted where the bytes actually
  move — per stream read and per successful stream-close commit (quota-refused
  commits count nothing)

Per-plugin admin dashboard (Phase 4 UI):

- Request count, error rate, p50/p95/p99 latency
- Memory high-water mark
- Capability denial events (security signal)

## 13. Security Review Checklist

- [ ] No `wasi_snapshot_preview1` exposed
- [ ] All SQL parsed and table-allowlisted before execution
- [ ] All filesystem paths normalized + jailed to user/system root before any storage op
- [x] All HTTP outbound URLs resolved + IP-checked against host allowlist (block private
      IP ranges unless explicitly granted) — enforced at dial time via
      `net.Dialer.Control` on the resolved IP (loopback/private/link-local/unspecified
      refused), opt out per plugin with `http.outbound_allow_private` (ADR-0057)
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

- `internal/plugins/host.go` — wazero runtime initialization
- `internal/plugins/abi.go` — `ncgo.log` only (proves the host call mechanism works)
- `internal/plugins/manifest.go` — TOML parser for `plugin.toml`
- `pkg/pluginsdk/` — Go bindings for `Info`/`Warn`/`Error` only
- `examples/hello-plugin/` — TinyGo plugin that logs "hello from wasm" on `ncgo_on_install`
- Integration test: load → install → verify log line → unload

Full ABI implementation is the bulk of Phase 4.

## Change Log

- **2026-09-23** — Phase 4v re-scoped the three §8 open-handle budgets from
  per instance to **per plugin, shared across instances** (ADR-0068),
  closing the ADR-0060 aggregate-cap follow-up. The budget values are
  unchanged (64 stream / 16 DB rows / 16 HTTP response) but now bound one
  plugin's handles aggregated across all of its live instances, so the
  pooled and per_request models can no longer multiply a plugin's
  open-handle footprint — open rows handles pin `database/sql` pool
  connections — by `pool_size` or request concurrency. The host keeps a
  per-plugin `(plugin id, handle kind) → count` aggregate acquired in
  `handleTable.add` (per-table check first, so neither refusal path leaks a
  slot) and returned in `remove`/`closeAll`, with entries deleted at zero;
  a trapped instance therefore returns its slots exactly, and handle ids
  stay instance-scoped. Exhaustion still answers `ErrUnavailable` (-12)
  from the same calls, so guests need no change. The manifest validation
  also gains the missing `pool_size` upper bound: pooled plugins now
  require `pool_size` in 1..32 (each pooled instance is a live wasm module
  with its own linear memory). ADR-0060's statement slots and the new
  aggregate govern different dimensions — in-flight statement execution vs
  retained open handles — and both remain in force. No new config keys or
  metric families; refusals land in the existing §12 `result` label.
- **2026-09-23** — Phase 4u implemented `storage_mkdir` and per-plugin
  storage byte metrics (ADR-0067), closing two ADR-0042/0061 follow-ups. The
  new §6.3 host function `storage_mkdir(path_ptr, path_len) -> i32` creates a
  single directory (no implicit parents) under the write capability of its
  scope: user scope commits through the DAV (filecache-consistent,
  incoming-mount aware), system scope through the plugin's system tree; an
  existing target answers -5, a missing parent -4 (`localfs.Mkdir` now maps
  that to the `storage.ErrNotFound` sentinel like `Stat`/`Open` do), and
  directories carry no bytes so no quota applies. The §12 observability
  surface gains `ncgo_plugin_storage_bytes_total{plugin, op, scope}` (op ∈
  read|write, scope ∈ user|system) backed by a new `Registry.AddCounter`
  (non-positive deltas are no-ops); count points sit where bytes actually
  move — per `storage_stream_read` read (the open handle now carries its
  scope) and per successful stream-close commit (user spool / system write;
  quota-refused commits count nothing) — with the 4i nil-registry zero
  overhead and the 4n/4o plugin-id nil-guard. The third storage follow-up,
  chunked/resumable plugin writes, is conditionally deferred: the spool
  covers single files to 1 GiB under quota, plugin state is small, the DAV
  chunked machinery serves human large-upload clients, and a plugin-side
  upload_init/chunk/finish ABI costs more surface than v1 has use for — it
  reopens on a concrete plugin need. Adding functions within `ncgo-abi/1` is
  permitted by §9. The pluginsdk gains `StorageMkdir`.
- **2026-09-23** — Phase 4t implemented outbound request body streaming
  (ADR-0066), closing the ADR-0043 ">1 MiB uploads" follow-up. Three new
  §6.3 host functions stage a body in a temp-file spool:
  `http_request_body_create() -> i64` (gated on `http.outbound` like
  `http_request` itself), `http_request_body_write(handle, buf_ptr,
  buf_len) -> i32` (chunk append; -11 above the 1 MiB `buf_len` or past the
  spool cap), and `http_request_body_close(handle) -> i32` (seals; writes
  and repeated closes after the seal → -2). The spool cap reuses
  `HostConfig.MaxSpoolBytes` (default 1 GiB, the storage-spool knob), and
  the handle shares the §8 64-stream budget. The `http_request` map gains
  `body_handle`, mutually exclusive with `body_bytes` (both → -2); the
  handle must be sealed (unsealed → -2, stays usable), consumption is
  destructive — the request goes out with a known `ContentLength` and a
  `GetBody` that reopens the spool for 307/308 replay, and the spool file
  is deleted when the call ends, success or failure (never-consumed spools
  die with the instance handle-table cleanup). Allowlist, egress IP guard,
  rate limit, redirect re-validation, and the response byte cap apply to
  the streamed path unchanged, and allowlist/rate denials run before
  consumption so a throttled guest keeps its spool. Spool over live
  streaming (`io.Pipe`) because the guest only runs inside host calls while
  `client.Do` reads the body asynchronously, the per-call timeout model has
  no clean owner for a cross-call upload, and a mid-upload upstream failure
  cannot travel back to a guest that already handed the bytes off. Adding
  functions within `ncgo-abi/1` is permitted by §9. The pluginsdk gains
  `HTTPBodyCreate`/`HTTPBodyWrite`/`HTTPBodyClose` and a `BodyHandle` field
  (`omitempty`) on `HTTPOutboundRequest`.
- **2026-09-23** — Phase 4s implemented the §7 `body_handle` request map as
  a manifest opt-in (ADR-0065): plugins with
  `runtime.request_body_stream = true` no longer have their route request
  body pre-read; dispatch wraps `req.Body` as a stream handle on the
  instance's `handleStream` table (sharing the §8 64-stream budget) and the
  map carries `{method, path, headers, query, body_handle}` with no inline
  `body_bytes`, so the 1 MiB inline cap (and its 413) does not apply. The
  new §6.3 host functions `request_body_read`/`request_body_close` mirror
  `http_response_body_read`/`http_response_close` (bytes read >0, 0 at EOF,
  -11 above the 1 MiB `buf_max`, -4 missing handle, -2 foreign handle type);
  they are always granted — the body is the plugin's own inbound request.
  The handle is registered before `ncgo_on_request` runs, may be closed
  early by the guest, and is otherwise dropped by handle-table cleanup when
  the instance is released; `req.Body` itself stays owned by net/http (the
  handle's Close is a no-op, never a double close). Plugins without the
  opt-in get byte-identical legacy behavior (inline `body_bytes`, 413 above
  1 MiB). Adding functions within `ncgo-abi/1` is permitted by §9. The
  pluginsdk gains `RequestBodyRead`/`RequestBodyClose` and a `BodyHandle`
  field on `HTTPRequest`. Outbound streaming (>1 MiB `http_request`
  uploads) remained a follow-up, closed by Phase 4t (see the 4t entry
  above).
- **2026-09-22** — Phase 4q2 closed the hot-reload × cache-cleanup
  interaction (ADR-0063): the reconciler purges a plugin's `plugin:<id>:`
  cache keys when a stop is an uninstall (registry row gone), and keeps them
  on disable/upgrade — covering memory deployments the CLI-side purge
  (ADR-0058) cannot reach.
- **2026-09-22** — Phase 4q added plugin hot reload (ADR-0062), closing the
  ADR-0041 "routes mount at boot; runtime refresh is future work" follow-up:
  install/enable/disable/upgrade through `ncgo-cli` now take effect on the
  running server within `plugin.refresh_interval` (default 10s; 0 disables
  the poll and restores restart-only semantics, negatives rejected). A new
  `plugins.Reconciler` polls the registry (`Sync` is one idempotent pass,
  `Run` the blocking loop, `Close` stops everything for `App.Close`): a
  plugin enabled but not running is started and its routes mounted, a
  running plugin no longer enabled — or whose registry `version` changed
  (upgrade = stop + start; the CLI's clear-then-hook already rewrote the
  route rows) — is unmounted and stopped. Stop order is tracked-route
  removal first, then the `plugin.<id>` job adapter is unregistered from the
  runner (the new `jobs.Runner.Unregister`), then `Plugin.Close` (which
  detaches event delivery); rows queued while a plugin is down retry as
  unknown-name and are dropped after three strikes. Removal works from the
  route keys recorded at mount time because an uninstall has already deleted
  the registry's route rows by the time it is reconciled. `httpx.Router`
  gained a `sync.RWMutex` (handlers are resolved under one read lock and
  invoked after unlocking) and exact-route `Remove`. Per-plugin failures stay
  isolated the startOne way (log, skip, retried next pass); a registry read
  failure keeps the current running set. In-flight requests are unaffected
  by unmounting — they already hold the handler — and a same-version
  reinstall is not detected (version is the change signal). WebDAV prop
  registrations needed nothing: they are looked up per request.
- **2026-09-22** — Phase 4p closed the ADR-0042 "quota checks" follow-up
  (ADR-0061): both `storage_*` write paths now enforce quotas at two
  checkpoints — `storage_create` against the declared size (skipped when the
  guest passes a size `<= 0`, meaning unknown) and `storage_stream_close`
  against the actual bytes. User scope checks the calling user's
  `users.quota_bytes` (NULL = unlimited) against the filecache usage via the
  new `files.DAV.Usage` pass-through; system scope checks the plugin's
  `<SystemPrefix>/<id>` tree against `HostConfig.PluginSystemQuotaBytes`
  (default 1 GiB, operator-tunable via `plugin.system_storage_quota_mb`, 0 =
  host default, negatives rejected), with the tree size recomputed per
  create by a bounded recursive `List` walk. Overwrites pay only the delta:
  the refusing condition is `usage - oldSize + newBytes > quota` with the
  pre-existing target's `Stat` size as `oldSize` (0 when absent). Refusals
  return -8 (`ErrQuotaExceeded`) plus a warn log naming the plugin — the
  4n/4o posture, classified into the existing bounded `result` label with no
  new metric families — and a system-scope refusal at close additionally
  removes the just-committed partial content (backend creates are atomic;
  an overwrite refusal loses the replaced old content, so guests must treat
  -8 from `storage_stream_close` as "the write did not happen"). Enforcement
  is best-effort: check and commit are not transactional, and the core DAV
  write path itself remains quota-free for non-plugin callers — core DAV
  quota enforcement is a documented follow-up alongside chunked/resumable
  writes, per-plugin storage byte metrics, and a mkdir host function.
- **2026-09-22** — Phase 4o closed the ADR-0039 "per-plugin connection
  pools/quotas" follow-up in its quota form (ADR-0060): the five
  connection-consuming `db_*` entries (`db_query`, `db_exec`, `db_tx_query`,
  `db_tx_exec`, `db_tx_begin`) now acquire a per-plugin-id in-flight
  statement slot after the capability/SQL checks and hold it only for the
  duration of the `Query`/`Exec`/`Begin` call; exhaustion returns -8
  (`ErrQuotaExceeded`) plus a warn log naming the plugin.
  `HostConfig.DBMaxConcurrentPerPlugin` defaults to 4 and is
  operator-tunable via `plugin.db_max_concurrent_per_plugin` (0 = host
  default, negatives rejected). Open rows/tx handles remain governed by the
  §8 per-instance handle budgets — the quota bounds concurrent *execution*,
  not handle lifetime, and a cross-instance aggregate handle cap stays a
  follow-up. No new metric families: refusals classify into the existing
  bounded `result` label (-8 → `internal`), and statement duration was
  already covered by the Phase 4i §12 histograms, closing the "query
  duration" follow-up without a code change. Read/write splitting remains
  the last open ADR-0039 follow-up.
- **2026-09-22** — Phase 4n closed the ADR-0043 "per-plugin rate limits" and
  "response size caps" follow-ups (ADR-0059), leaving only >1 MiB streaming
  request bodies outstanding (closed by Phase 4t, see the 2026-09-23 4t
  entry). Each `http_request` call (redirect chain
  included) now draws one token from a per-plugin-id stdlib token bucket —
  `HostConfig.HTTPRatePerMinute` default 120, burst 30 — and exhaustion fails
  the call with -8 (`ErrQuotaExceeded`) plus a warn log naming the plugin;
  buckets are process-local and reset on restart. Response bodies are capped
  by `HostConfig.MaxHTTPResponseBytes` (default 32 MiB) via a counting body
  wrapper: the `http_response_body_read` that would push delivered bytes past
  the cap fails loudly with -11 (`ErrTooLarge`) — never a silent truncation —
  later reads keep failing, and status/header reads are unaffected. Both
  limits apply to the guarded and unguarded ADR-0057 clients alike and are
  operator-tunable through the new `plugin.http_rate_per_minute` /
  `plugin.max_http_response_mb` config keys (0 = host default, negatives
  rejected). No new metric families: the ADR-0055 wrapper classifies -11 as
  `too_large` and -8 as `internal` in the existing `result` label. §6
  documents the semantics.
- **2026-09-22** — Phase 4m closed the cache-cleanup follow-up Phase 4j left
  open ("cache keys under `plugin:<id>:` are NOT removed on uninstall
  (`cache.Cache` has no prefix delete)"). `cache.Cache` gains
  `DeleteByPrefix(ctx, prefix) (int64, error)` (ADR-0058) with an
  empty-prefix guard, implemented for Memory (tracked-key index over
  ristretto, which has no key iteration), Redis (SCAN + glob-escaped MATCH +
  non-blocking UNLINK), and Tiered (both layers, L2-authoritative count).
  `Installer.Uninstall` now purges the plugin's `plugin:<id>:` namespace
  after the appconfig step and logs the count; `ncgo-cli` injects a Redis
  cache for this step only when `cache.redis_addr` is configured — with a
  memory-only deployment the residue dies with the server restart uninstalls
  already require. The ABI itself is unchanged; `cache_*` key namespacing
  (§5) already used the `plugin:<id>:` prefix this step deletes by.
- **2026-09-22** — Phase 4l closed the §13 private-IP egress gap (ADR-0057).
  The `http.outbound` allowlist is hostname-based, so literal internal IPs
  (e.g. the cloud metadata endpoint 169.254.169.254) and DNS rebinding (a
  granted hostname resolving to 127.0.0.1) bypassed it. Outbound dials now
  install a `net.Dialer.Control` hook that checks the **resolved** IP — no
  TOCTOU window, covering literal-IP URLs and DNS answers at the same point —
  and refuses loopback, private (RFC1918 + ULA fc00::/7), link-local
  unicast, and unspecified addresses (IPv4-mapped forms unmapped first;
  CGNAT 100.64.0.0/10 and multicast deliberately NOT blocked, Tailscale and
  carrier deployments use them legitimately). Because Control sees no
  request context, per-plugin authorization happens at client selection: the
  host builds a guarded clone of `HostConfig.HTTPClient` and plugins granted
  the new manifest boolean `http.outbound_allow_private = true` use the
  configured client as-is; the two clients sit on separate transports, so
  connections pooled to private targets are never shared across
  authorization levels. Escape hatches: a custom RoundTripper or an
  operator-set Dial/DialContext disables the guard (debug log; the operator
  client then owns egress policy), and proxied deployments see the proxy's
  address, not the target's — private-ingress proxies need a custom client.
  Guard refusals map to -3 (`ErrPermissionDenied`) with a warn-level
  security log naming the plugin. The §13 "URLs resolved + IP-checked" item
  is now checked; §5 and §6 document the new grant.
- **2026-09-22** — Phase 4j closed three lifecycle gaps (ADR-0056).
  **(G1) The CLI install-time host is now the full host.** `ncgo-cli plugin
  install`/`uninstall` construct the plugin host with every subsystem the
  server wires in `app.New` — DB, route/OCS/WebDAV-prop registry,
  appconfig store, jobs runner, event bus, files DAV, and system storage
  under `appdata_<instance.id>/plugins` (with the server's ephemeral
  `"oc"+hex` fallback when `instance.id` is unset) — so lifecycle hooks can
  use every granted capability instead of failing with -12. Cache is
  deliberately absent (the CLI is a management surface and cache is runtime
  state: `cache_*` in hooks gets -12); metrics stay off. `plugin check`
  keeps its empty host (compile/ABI smoke test only; documented in
  `examples/file-tagger`). The CLI's jobs runner is constructed but never
  started — hook-enqueued rows wait for the next server boot — and
  install/upgrade now register the plugin's `plugin.<id>` job adapter
  before the hook runs, so `job_enqueue` works at install time (previously
  it failed with `ErrUnknownJob` until the next boot's adapter
  registration). pluginsdk gains the missing RouteRegister/OCSRegister
  bindings. **(G2) Upgrades run `ncgo_on_upgrade`.** Upgrade ordering is
  *clear-then-hook*: after the signature/capability gates and BEFORE the
  new archive is stored, the installer deletes the old version's
  `plugin_routes`/`plugin_webdav_props` rows (upsert-only registrations no
  longer linger from versions that stopped declaring them), starts the
  plugin, and invokes §7's `ncgo_on_upgrade(from_version_ptr,
  from_version_len)` as a lifecycle hook (DDL and hook-only registrations
  permitted, exactly like `on_install`); a failing hook leaves the old
  archive installed. Plugins without an `on_upgrade` entry point fall back
  to `on_install`, so route-registering plugins keep working. Fresh
  installs are unchanged (`on_install` only). **(G3) Uninstall cleans up.**
  Uninstall now also deletes the plugin's `plugin.<id>` jobs rows
  (`jobs.Store.DeleteByName`), its `("plugin", <id>.*)` appconfig rows
  (`appconfig.Store.DeleteByPrefix` with LIKE wildcards escaped via
  `ESCAPE '!'`), and its `<SystemPrefix>/<id>/` system-storage tree
  (bounded recursive walk over List/Delete: depth ≤ 64, ≤ 100k entries,
  missing root tolerated). Runner safety net: rows whose job name no
  runner knows are failed-and-rescheduled at most
  `maxUnknownJobAttempts` (3) times, then completed with a warn log —
  ending the infinite retry loop for leftovers from before this cleanup
  existed. **Newly confirmed deviations (from review):**
  `runtime.fuel_per_call` is parsed but unenforced (wazero v1 has no
  fuel-metering API; CPU budget remains wall-clock timeout only);
  per-plugin `runtime.memory_limit_mb` is validated but not applied (memory
  is capped host-wide via `DefaultMemoryLimitMB`); §8 trap-during-request
  returns **502**, not 500 (deliberate — 502 marks plugin failure vs core
  failure; §8 text corrected); the §13 private-IP egress check remains
  pending (follow-up). **Remaining limitation:** cache keys under
  `plugin:<id>:` are NOT removed on uninstall (`cache.Cache` has no prefix
  delete) — follow-up.
- **2026-09-22** — Phase 4i implemented §12 Prometheus metrics (ADR-0055):
  `ncgo_plugin_host_calls_total{plugin,function,result}` and
  `ncgo_plugin_host_call_duration_seconds{plugin,function}` are emitted for
  every host call (instrumented uniformly at the `registerHostModule` export
  helper), and capability denials additionally increment
  `ncgo_plugin_capability_denials_total{plugin,function}` as the security
  signal. The exposition registry is stdlib-only
  (`internal/observability`, no `client_golang`); `result` is a bounded
  classification of ABI codes (`ok` — including positive byte-count returns,
  `permission_denied`, `invalid_argument`, `not_found`, `unavailable`,
  `unsupported`, `too_large`, `timeout`, `canceled`, `internal`), never raw
  codes. `GET /metrics` is gated by `observability.metrics_enabled` (default
  off) with optional bearer-token auth (`observability.metrics_token`,
  constant-time compared). **Deferred:** OTel `plugin.host_call` spans (no
  OTel SDK in v1), the per-plugin admin dashboard (request count, error
  rate, p50/p95/p99, memory high-water), and guest entry-point call metrics.
- **2026-09-22** — Phase 4c8 implemented plugin WebDAV properties
  (ADR-0046): the §6.3 `webdav_register_prop` is live (lifecycle-hook only
  → -3; `webdav.props` grant required → -3; name must be `prefix:local`
  with a non-empty prefix and `local` matching `[A-Za-z][A-Za-z0-9_-]*` →
  -2; getter required and setter — when given — must be identifier-ish
  `[A-Za-z_][A-Za-z0-9_.]*` ≤ 128 → -2; nil registry → -12; an empty setter
  registers a read-only property). Registrations persist to
  `plugin_webdav_props` (migration `0018`) and are deleted on uninstall.
  **Namespace URI scheme (v1):** each plugin's props are emitted and
  patched under the per-plugin namespace URI
  `http://ncgo.local/ns/plugin/<plugin_id>` with the local name being the
  part after `:` in the registered name, so same-named props from
  different plugins never collide; the registered prefix is a
  capability-namespacing device and does not appear on the wire.
  **Getter/setter signature convention (v1, deviation from §7's i64
  MessagePack sketch):** the getter is
  `<getter>(path_ptr, path_len, out_ptr, out_max) -> i32` — the guest
  writes the raw string value (not MessagePack) into a host-allocated
  4 KiB buffer and returns the byte count (0 = empty, negative = error
  code); the setter is `<setter>(path_ptr, path_len, val_ptr, val_len) ->
  i32` (0 = OK, else HTTP 500). Emission is `<x:NAME
  xmlns:x="NS">value</x:NAME>` inline-xmlns style on PROPFIND;
  getter/trap failures omit the prop and never fail the response;
  PROPPATCH routes to the setter first (403 for read-only or detached
  plugin) and falls through to core behavior when the (ns, name) pair is
  not a registered plugin prop; a remove op is delivered as an empty
  value. pluginsdk gains WebDAVRegisterProp/WebDAVPropArgs. Follow-ups:
  allprop performance/batching, prop caching, 404 propstat for
  requested-but-unset plugin props.
- **2026-09-22** — Phase 4c7 implemented plugin jobs (ADR-0045): the §6.3
  `job_enqueue` is live and §7's `ncgo_on_job` is delivered. **Namespacing
  (v1):** every plugin's jobs run under a single adapter job
  `plugin.<plugin_id>`; the row payload is a MessagePack
  `{name: string, payload: []byte}` envelope and the guest's `on_job`
  receives the plugin-local name and payload verbatim. **Validation (v1):**
  name must be 1–128 bytes of `[A-Za-z0-9_.-]` (else -2); `jobs.register`
  required (-3); nil runner → -12; enqueue without an `on_job` entry point
  → -2 (the row could never be delivered); `run_at_unix_ms` ≤ 0 or past
  means now, more than ten years out → -2; payload capped at 1 MiB; runner
  errors → -1. **Delivery semantics (v1):** the adapter drops (runner
  completes) undeliverable rows — detached plugin, malformed envelope, no
  entry point — and returns an error (runner retries at now+poll) on guest
  failure (non-zero i32 or trap). Adapter registration happens at plugin
  start (`startOne`) with duplicates tolerated and failures logged, never
  failing boot. `on_job` runs without a user context (user-scope storage
  unavailable); HostConfig gains Jobs; pluginsdk gains JobEnqueue/JobArgs.
  Follow-ups: envelope user field, scheduled recurring plugin jobs,
  uninstall-time row cleanup.
- **2026-09-22** — Phase 4c6 implemented plugin config (ADR-0044): the §6.3
  `config_get` / `config_set` pair is live. Storage is a generic
  `appconfig(appid, configkey, configvalue)` table (migration `0017`; the
  `oc_appconfig` analogue, reusable by core/Admin UI); plugin rows use
  `appid = "plugin"` and the `plugin.<plugin_id>.` namespace is realised as
  `configkey = <plugin_id>.<key>`, so cross-plugin reads are impossible
  even with a `*` grant. **Semantics (v1):** capability globs match the
  plugin-local key; empty key → -2; missing key → -4 (`ErrCodeNotFound`,
  same as cache/storage misses); nil store → -12; values are capped at
  64KiB (`maxStringArg`) on set → -11, and `config_get` into a too-small
  buffer → -11 with nothing written. HostConfig gains AppConfig; pluginsdk
  gains ConfigGet/ConfigSet bindings. Follow-ups: `config_delete`,
  list/keys, uninstall row cleanup, encryption-at-rest for secrets.
- **2026-09-22** — Phase 4c5 implemented outbound HTTP (ADR-0043): the
  §6.3 `http_*` family is live. `http_request` takes the MessagePack
  `{method, url, headers, body_bytes, timeout_ms}` map (gaining a
  `body_handle` alternative to inline `body_bytes` in Phase 4t, see the
  2026-09-23 4t entry); method must be
  GET/HEAD/POST/PUT/DELETE/PATCH/OPTIONS and the URL `http`/`https` with
  a non-empty host (else -2). **Allowlist semantics (v1):** the target is
  normalized to lowercase `host` when the port is the scheme default
  (80/443) or absent, else `host:port`; grants match exactly and
  case-insensitively — `example.com` covers default ports only,
  `example.com:8080` matches exactly, subdomains are never implied.
  Every redirect target is re-validated against the same allowlist on a
  per-request shallow client copy (-3 on a non-granted hop); plugin
  `Host` headers are dropped; `timeout_ms` ≤ 0 defaults to 10s and is
  clamped to 30s (deadline → -6, cancel → -7, other network errors → -1).
  Responses stream through per-instance handles on the 16-entry
  `handleHTTP` budget (-12 beyond): `http_response_status` returns the
  status code, `http_response_header` joins values with `", "` (absent →
  0 bytes), `http_response_body_read` returns 0 at EOF,
  `http_response_close` releases the body; stale handles → -4 and leaked
  responses are closed by handle-table cleanup.
- **2026-09-22** — Phase 4c4 implemented storage (ADR-0042): the §6.3
  `storage_*` family is live. **Path convention (v1):** plugin storage
  paths carry a scheme prefix — `user:/path` addresses the calling user's
  files, `system:/path` addresses the plugin's system storage, bare paths
  default to `user:`; after scheme strip the remainder is normalized
  (`..`/empty segments/NUL rejected, -2), unknown schemes are rejected
  (-2), and `/` (scope root) is valid only for stat/list. User-scope
  operations run through the files DAV as the call's user
  (`CallContext.UserID`, empty → -12) so filecache/etag/trash/versions
  stay consistent; user writes spool to a temp file and commit one-shot
  through `DAV.Write` on `storage_stream_close` (1 GiB spool cap, -11;
  leaked spools are discarded by closeAll). System paths resolve to
  `appdata_<instanceID>/plugins/<plugin_id>/<path>` on the default storage
  backend. `storage.read`/`storage.write` scope lists gate each operation
  per scope; streams share the 64-handle `handleStream` budget. The stat/
  list MessagePack entry shape is `{path, size, mtime_unix_ms, is_dir}`.
  `events.Event` carries an optional `UserID` (set by `files.uploaded`)
  which the dispatcher adopts as the delivery's call user when the publish
  context has no explicit call metadata.
- **2026-09-22** — Phase 4c3 implemented plugin HTTP routes (ADR-0041):
  §6.3 `route_register`/`ocs_register` are live (lifecycle-hook only,
  per-plugin `/apps/<id>/` namespace, persisted in `plugin_routes`, upsert
  semantics, deleted on uninstall) and §7 dispatch mounts persisted records
  at boot on the app router — plain routes behind the DAV auth chain, OCS
  endpoints under both `/ocs/v1.php` and `/ocs/v2.php` with JSON-body
  envelope wrapping. **Spec deviations:** the §7 request map carries
  `body_bytes` inline (≤ 1 MiB, 413 above) unless the plugin opts into the
  specced `body_handle` map via `runtime.request_body_stream` (opt-in added
  in Phase 4s, see the 2026-09-23 entry), and
  `ncgo_response_header_at` lines use the `"Name: Value"` format (host
  splits on the first `": "`, ignores plugin Content-Length).
- **2026-09-22** — Phase 4c2 implemented events (ADR-0040): §6
  `event_publish` and the §6/§10 subscription model are live — synchronous
  in-process bus, `events.subscribe` globs gate `ncgo_on_event` delivery
  (alloc/write/call/free into guest memory), publishers skip themselves,
  delivery failures never fail the publish, and the files module emits
  `files.uploaded` with a MessagePack `{user, path, size, created}` payload
  per §10.
- **2026-09-21** — Phase 4c1 implemented `db.*` (ADR-0039): §14 Q1
  resolved with xwb1989/sqlparser (single MySQL-dialect parser for all
  three backends, fail closed); DDL restricted to lifecycle hooks per
  §6.3; transaction handles share the rows budget; rows are MessagePack
  arrays per §6.1 via vmihailenco/msgpack/v5.
- **2026-09-21** — Phase 4b pinned the §4 archive format and §13 signature
  scheme (ADR-0038): signed payload = canonical JSON array of
  `{name, sha256}` sorted by name; `signature.sig` = JSON
  `{keyid, base64-ed25519}`; trust model = operator-pinned keys in
  `<install_dir>/trusted_keys/*.pub` (resolves §14 Q2 for v1; TUF/CA
  deferred).
- **2026-09-21** — Phase 4a runtime core landed (ADR-0037): typed
  capabilities, instance models, and the full host function surface are
  implemented per this spec with one deviation — routes/ocs manifest
  validation requires the `/apps/` prefix only, because the walkthrough in
  §10 grants `/apps/file-tagger/` for plugin id `com.example.file-tagger`
  (strict `<plugin-id>` prefix would reject it). blake2b is BLAKE2b-256.
- **2026-09-17** — Phase 0 stub lives in `internal/plugins`. wazero v1 has no
  fuel metering API; CPU budget is wall-clock timeout + memory pages until
  Phase 4 (ADR-0006).
- **2026-04-29** — Initial spec committed.
