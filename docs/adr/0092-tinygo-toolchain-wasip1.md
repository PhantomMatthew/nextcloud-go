# ADR-0092: Phase 5s TinyGo toolchain — wasip1 reactor target + real pluginsdk bindings

- **Status**: Accepted
- **Date**: 2026-09-25
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0003 (the "TinyGo bindings" claim was aspirational — the
  SDK's guest side never compiled before this phase), ADR-0037 (its §5/§6
  "pluginsdk under `//go:build wasm`" and wasmgen-only testing posture),
  ADR-0056 (the recorded "wasm-unknown" build assumption)

## Context

Until this phase no machine in the loop had TinyGo installed, so all plugin
tests ran against hand-assembled wasmgen probe modules (ADR-0037 §6). That
covered the host side only. When TinyGo 0.42.0 finally built
`examples/hello-plugin`, four independent defects surfaced in sequence, all
invisible without a real compiler:

1. **The SDK build tags were wrong.** `pkg/pluginsdk` split bindings as
   `*_wasm.go` (`//go:build wasm`) vs `*_stub.go` (`!wasm`). TinyGo's
   wasm-unknown target declares `GOOS=linux GOARCH=arm` with build tags
   `tinygo.wasm`/`wasm_unknown` — the `wasm` tag is never set, and the
   `_wasm.go` filename suffix (an implicit GOARCH=wasm constraint) never
   matches either. Every historical artifact compiled the **stubs**: they
   were hollow shells with no ABI calls and no exported `ncgo_alloc`/
   `ncgo_free`/`ncgo_abi_version`. Nothing ever noticed because no host
   ever loaded a real build.
2. **`ctx` bindings passed wasmimport functions as values**, which TinyGo
   forbids — latent compile error behind the tag bug.
3. **pluginsdk pulls in `msgpack/v5`**, whose generated decoder contains a
   `go` statement (dead code for our decoder flags), and wasm-unknown's
   default `scheduler=none` rejects goroutine statements at compile time.
4. **Building with `-scheduler=asyncify` compiles but cannot run here**:
   the asyncify wasmexport trampoline drives TinyGo's task scheduler through
   `env.tinygo_launch`/`env.tinygo_rewind` imports — host hooks that must
   switch the linear-stack pointer and `call_indirect` a table entry
   (TinyGo PR #4451). wazero's public API exposes neither. wasm-unknown +
   asyncify is therefore unrunnable for this host, now and foreseeably.

## Decision

1. **Build target: wasip1 reactor mode.** Plugins build with
   `tinygo build -target=wasip1 -buildmode=c-shared -no-debug`. On wasip1
   the asyncify scheduler is fully self-contained in the module (no host
   hooks), `GOARCH=wasm`, and `_initialize` + `//go:wasmexport` exports
   follow the reactor convention. The Makefile's example targets and the
   e2e test use these flags.

2. **WASI import allowlist at load.** The host instantiates wazero's WASI
   preview1 module so wasip1 reactors link, but `Load` admits only:
   `fd_write`, `poll_oneoff`, `clock_time_get`, `args_sizes_get`,
   `args_get`, `random_get`. Every other WASI name — `fd_read`,
   `path_open`, `proc_exit`, `environ_get`, … — is `ErrForbiddenImport`.
   No stdin, filesystem, sockets, or environment for guests; stdout/stderr
   writes are discarded by module config. (Spec §8 updated; the
   import-guard error now names `module.name`.)

3. **`_initialize` once per instance.** `instanceManager.instantiate`
   calls the reactor init export when present, before any other export;
   wasmgen probe modules don't export it and are unaffected.

4. **pluginsdk tags flipped to `tinygo`.** All 17 binding files renamed
   `*_wasm.go` → `*_tinygo.go` with `//go:build tinygo`; stubs now
   `//go:build !tinygo`. The `_wasm.go` suffix is dropped because it is an
   implicit GOARCH constraint that wasm-unknown (GOARCH=arm) never
   satisfies. The `ctx` bindings' function-value helper was inlined per
   call site.

5. **End-to-end test, gated on the toolchain.**
   `internal/plugins/tinygo_e2e_test.go` compiles `examples/hello-plugin`
   from source with the real toolchain, loads it, and asserts the
   `pluginsdk.Info` log line crosses the ABI. It skips when `tinygo` is
   not on PATH, so toolchain-less CI stays green. wasmgen probes remain
   the host-side unit-test strategy; the policy probes changed with the
   guard: `fd_write` (allowed) now reaches the missing-export check, and a
   new `path_open` probe pins the filesystem rejection.

## Alternatives Considered

### Stay on wasm-unknown and stub `env.tinygo_launch`/`tinygo_rewind` as traps
- Pros: no target change; asyncify builds load when goroutines are never
  used.
- Cons: the premise is false — with asyncify, **every** wasmexport call
  goes through the scheduler trampoline, so the trap fires on every entry
  point. Implementing the hooks for real is impossible through wazero's
  public API. Rejected (verified empirically: exit-code trap on
  `ncgo_on_install`).

### Hand-roll a msgpack codec guest-side to survive `scheduler=none`
- Pros: keeps wasm-unknown; zero goroutine pressure.
- Cons: re-implements a wire format against six call sites including
  open-ended `[]any` (db args/rows, timestamps via ext) — a correctness
  risk for the ABI itself, to dodge a toolchain flag. Rejected.

### Admit all of WASI preview1 instead of an allowlist
- Pros: nothing to maintain.
- Cons: hands plugins `path_open` (host filesystem under the process's
  permissions) — indefensible for a sandbox. Rejected.

## Consequences

- Plugin examples are real for the first time: the rebuilt artifacts carry
  the genuine SDK bindings (hello.wasm went from a 2.7 KB hollow stub to a
  ~122 KB real module).
- Plugin authors need TinyGo and the pinned build flags (Makefile +
  READMEs + spec §10 updated).
- The WASI allowlist is a new security boundary; widening it is an ADR.
- `time.Sleep` inside a plugin blocks the calling instance for its
  duration (poll_oneoff/clock are allowlisted); the per-call wall-clock
  timeout bounds it as before.
- wazero stays the only wasm dependency; zero new module requirements.

### Follow-ups (explicit, not in v1)

- ~~Route plugin stdout/stderr (`fd_write`) into the plugin log stream
  instead of discarding.~~ (**resolved by ADR-0093**)
- Evaluate `wasm-unknown` again only if wazero grows the host-hook surface
  (stack-pointer globals + table calls).

## Verification

- `TestTinyGoHelloPluginEndToEnd`: real compile → load → install → host
  log assertion (skipped without tinygo).
- `TestHostForbiddenImport` (path_open rejected) /
  `TestHostAllowedWASIImport` (fd_write passes the guard, fails later at
  missing exports).
- `make example-plugins` builds all three examples; full suite +
  `go test -race` green; `golangci-lint run ./...` 0 issues;
  `go mod tidy` no diff (WASI import ships inside wazero).
