# ADR-0093: Phase 5t plugin stdout/stderr routed into the host log stream

- **Status**: Accepted
- **Date**: 2026-09-25
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0092 (resolves its stdio-routing follow-up)

## Context

ADR-0092 put plugins on the wasip1 reactor profile and allowlisted
`fd_write`, but the module configuration never wired stdout/stderr anywhere,
so wazero discarded them: anything a guest prints — `fmt.Println`, `println`,
TinyGo runtime diagnostics — vanished. Structured logging goes through the
`ncgo.log` host function, yet an author reaching for `fmt.Print` while
debugging gets silence, and the TinyGo runtime's own panic output on stderr
is lost (the accompanying trap is logged, the runtime's message is not).

## Decision

Attach a per-instance, line-buffering writer to the wazero module
configuration (`WithStdout`/`WithStderr`, `internal/plugins/stdiolog.go`)
that routes `fd_write` bytes into the host slog stream:

- One record per complete line: **stdout at Info, stderr at Warn**. Stdio is
  unstructured diagnostics; guest code that wants Error has the `ncgo.log`
  level for it.
- Attributes mirror `ncgo.log` (`plugin.id`, `plugin.version`) plus a
  `plugin.stdio` discriminator (`stdout`/`stderr`).
- Partial lines are buffered across writes and, for long-lived instances,
  across calls (a singleton's unterminated tail joins its next call's
  output); the remainder is flushed when the instance closes.
- Lines cap at 4096 bytes: an unterminated stream is force-emitted at the
  cap with a `…[truncated]` marker and buffering resumes from what follows;
  a newline-terminated line over the cap is truncated the same way. Blank
  lines are dropped and a trailing `\r` is stripped.
- Writes never fail: `fd_write` always reports success, so logging can never
  trap a guest.
- The writer sees no call context; attribution is by identity attrs only
  (same posture as the pool-replenish log path). Guest calls are serialized
  per instance in every instance model, so `Write` is effectively
  single-threaded; a mutex only covers the shutdown-time flush racing an
  in-flight call.

## Consequences

- `fmt.Println`/`println` and TinyGo runtime stderr output appear in the
  server log with plugin identity; no plugin, SDK, or manifest change.
- `ncgo.log` remains the structured, level-controlled channel; stdio is the
  zero-effort fallback.
- Buffer growth is bounded: each live instance carries at most one capped
  partial line.
- ABI spec §7 updated: stdio is logged, not discarded.

## Verification

- wasmgen `WASIFdWriteModule` drives real `fd_write` calls through
  Load+Install: split chunks reassemble into lines, stdout→INFO /
  stderr→WARN with the `plugin.stdio` attr, and an unterminated tail is
  flushed at instance close.
- Writer unit tests pin line splitting, `\r` stripping, blank-line dropping,
  cap truncation (forced and newline-terminated), flush idempotence, and
  nil-logger discard.
- Full suite + `go test -race` green; `golangci-lint run ./...` 0 issues;
  `go mod tidy` no diff (no new dependency).
