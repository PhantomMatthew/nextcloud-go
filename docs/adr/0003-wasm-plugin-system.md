# ADR-0003: WASM Plugin System for Third-Party Apps

- **Status**: 🟢 Accepted
- **Date**: 2026-04-29
- **Deciders**: Project lead

## Context

Upstream Nextcloud's value comes substantially from its **app ecosystem** — Talk,
Mail, Office, Forms, Deck, Notes, Calendar, Contacts, Photos, and hundreds of
community apps. A Go rewrite that cannot host third-party extensions would be a
strict downgrade for users no matter how good the core is.

ADR-0002 commits us to a greenfield architecture, which means we cannot load existing
PHP apps. We need a **new plugin model** designed for Go.

Constraints:

1. **No CGO** (per ADR-0001) — rules out anything requiring native runtimes (V8, JSC,
   wasmtime-c, etc.)
2. **Sandboxed by default** — third-party plugins must not be able to read arbitrary
   files, exec processes, or open arbitrary network sockets. Capability-based.
3. **Language-agnostic** — plugin authors should not be forced to write Go. We want
   Rust, AssemblyScript, TinyGo, Zig, C, etc. to be viable.
4. **Stable ABI** — plugins compiled today must run on hosts shipped years from now.
   The ABI is a public contract.
5. **Deterministic resource limits** — CPU (fuel metering), memory ceiling, syscall
   blast radius all bounded.
6. **Distributable** — plugins are single-file artifacts, signable, mirrorable.

Options surveyed:

- Go plugins (`plugin` package) — Linux-only, brittle, no sandbox, requires identical
  Go toolchain. Effectively unusable.
- Subprocess + RPC (gRPC over UDS, like HashiCorp's `go-plugin`) — mature, but no
  sandbox by itself, weak resource limits, OS-process overhead per plugin, deployment
  story is messy.
- Embedded scripting (Lua via gopher-lua, Starlark, Tengo, JS via goja) — single
  language only, weak ecosystem, no compiled-language perf.
- WASM via wazero — pure Go, sandboxed by construction, multi-language toolchains,
  resource limits, host-function pattern for capability injection.

WASM is the only option that satisfies all six constraints simultaneously.

## Decision

`nextcloud-go` ships a **WASM-based plugin system** built on
[`github.com/tetratelabs/wazero`](https://wazero.io).

Key elements (full design in [`docs/specs/wasm-plugin-abi.md`](../specs/wasm-plugin-abi.md)):

- **ABI version**: `ncgo-abi/1`. Plugins declare the ABI they target. The host
  refuses to load mismatched plugins.
- **Manifest**: each plugin ships a `plugin.json` (or embedded `_manifest` custom
  section) declaring id, version, ABI, capabilities requested, exported handlers.
- **Capability model**: default-deny. Plugins request capabilities (`fs.read:/...`,
  `db.query`, `http.outbound:host:port`, `events.subscribe:topic`, etc.) which the
  host operator approves at install time.
- **Host functions**: small, audited surface (`log`, `kv_get`, `kv_set`, `db_query`,
  `http_fetch`, `event_publish`, `secret_get`, etc.) injected per granted capability.
- **WASI is NOT exposed** to plugins. The host provides only `ncgo-abi/1` host
  functions. No filesystem, no clock skew, no environment variables, no command-line
  args from the WASI surface.
- **SQL safety**: plugin DB queries are parsed (`pg_query_go` for Postgres, Vitess
  parser for MySQL, custom shim for SQLite) and rejected if they touch tables outside
  the plugin's namespace prefix.
- **Resource limits**: per-call fuel budget (configurable), memory ceiling (default
  64 MiB), wall-clock timeout (default 5s for sync handlers, longer for jobs).
- **Plugin SDK**: `pkg/pluginsdk/` provides idiomatic Go (and TinyGo) bindings.
  Other-language SDKs (Rust, AS) are community-driven but specified by the ABI doc.

In Phase 0, the host implements **only the lifecycle + logger** host functions, enough
to load a hello-world plugin. The full ABI lands in Phase 4.

## Alternatives Considered

### Option A: Go `plugin` package
- Pros: Native Go, full speed
- Cons: Linux-only, no sandbox, requires byte-identical Go toolchain on host and
  plugin, breaks on every Go release, can't unload, no resource limits
- **Rejected** — operationally unusable

### Option B: Subprocess + RPC (HashiCorp `go-plugin` style)
- Pros: Mature pattern, language-agnostic via gRPC
- Cons: No sandbox without OS-level extras (seccomp/landlock/jails — platform-specific,
  CGO-adjacent, hard to ship cross-platform), one OS process per plugin (heavy at
  scale), weak/no fuel metering, plugin discovery and lifecycle messy
- **Rejected** — doesn't meet the sandbox-by-default requirement portably

### Option C: Embedded scripting (Lua/Starlark/Tengo/JS via goja)
- Pros: Lightweight, easy embedding
- Cons: Single language locks out the ecosystem, weak performance, weak typing,
  ecosystem libraries don't exist for the target languages, hard to enforce CPU limits
  on JS engines
- **Rejected** — language lock-in defeats the "language-agnostic" requirement

### Option D: WASM with wasmer-go or wasmtime-go
- Pros: Mature WASM runtimes, fast
- Cons: Both require **CGO**, violating ADR-0001
- **Rejected** — CGO

### Option E: WASM with wazero (chosen)
- Pros: Pure Go, no CGO, mature (Tetrate-backed), fuel metering, memory limits,
  custom host functions, WASI Preview 1 supported (we choose not to expose it),
  supports our toolchain matrix (Rust, TinyGo, AssemblyScript, Zig, C/Emscripten)
- Cons: Slower than CGO-backed runtimes (~2-5x for compute-heavy code; acceptable
  because plugins are typically I/O-bound glue code, not CPU kernels)
- **Chosen**

## Consequences

### Positive

- Single-binary host with a sandboxed extension model
- Multi-language plugin ecosystem from day one
- Capability-based security: plugin compromise is bounded
- Plugins are portable: same `.wasm` runs on Linux/macOS/Windows/ARM64/x86_64
- Plugins are signable, mirrorable, content-addressable
- Plugin authors get type-checked SDKs (Go, TinyGo, Rust at minimum)
- ABI versioning gives us a clean upgrade story

### Negative

- WASM has no native threading (Phase 0 ABI is single-threaded; revisit with WASM
  threads proposal later)
- WASM ↔ host data marshaling adds overhead (string copies, MessagePack for
  structured data); not suitable for tight inner loops
- ABI design is a long-lived public commitment. Mistakes are expensive to fix.
- We give up the ability to load existing PHP apps — there is no compatibility shim.
  Existing app authors must port.
- Plugin SQL parsing requires shipping multi-dialect parsers (`pg_query_go` is large,
  Vitess parser is large, SQLite parser is custom).

### Neutral / follow-ups

- ABI v2 (`ncgo-abi/2`) will likely adopt WASI Preview 2 / Component Model once
  toolchain support is broad. v1 stays supported indefinitely as a stable contract.
- A plugin **registry/marketplace** is out of scope for v1. Plugins are installed by
  URL or local file. Registry can be a Phase 5+ effort (or community-run).
- A reference plugin set will be published alongside the host (probably 2-3 simple
  plugins demonstrating each capability category) before declaring the ABI stable.

## References

- [`docs/specs/wasm-plugin-abi.md`](../specs/wasm-plugin-abi.md) — full ABI specification
- [`docs/plans/00-phased-rewrite-plan.md`](../plans/00-phased-rewrite-plan.md) — plugin work scheduled in Phase 4
- [ADR-0001](0001-tech-stack.md) — wazero selection rationale
- [ADR-0002](0002-greenfield-rewrite.md) — why we cannot load PHP apps
- [wazero docs](https://wazero.io/)
- [WASI Preview 1 spec](https://github.com/WebAssembly/WASI/blob/main/legacy/preview1/docs.md)
- [WASM Component Model](https://component-model.bytecodealliance.org/)
