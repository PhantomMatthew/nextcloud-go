# ADR-0059: Phase 4n Plugin HTTP Outbound Rate Limit + Response Size Cap

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none) — closes the ADR-0043 "per-plugin rate limits" and
  "response size caps" follow-ups

## Context

ADR-0043 gated plugin outbound HTTP on the `http.outbound` hostname
allowlist; ADR-0057 added the dial-time private-IP guard. Two listed
follow-ups remained: a runaway or hostile plugin could still (a) hammer a
granted host with unlimited request volume and (b) pull an arbitrarily large
response body into host memory/streaming buffers. `http_request` bodies are
already capped at 1 MiB by the shared payload limit, so the exposure is
inbound (responses) and rate, not outbound payload size.

## Decision

1. **Per-plugin rate limit.** `HostConfig.HTTPRatePerMinute` (<= 0 selects
   the default 120) backs a stdlib token bucket per plugin id
   (`internal/plugins/ratelimit.go`; the repo deliberately does not take on
   `x/time`). `httpRequest` draws one token after the capability and
   allowlist checks, before `client.Do`; one call — including its whole
   redirect chain — costs exactly one token. The burst capacity is
   `HostConfig.HTTPRateBurst`, default 30: short burst-style sync loops
   (fetch a dozen feeds at once) pass, and only sustained over-rate calling
   is throttled — the runaway/abuse case. Both clients (guarded and
   unguarded, ADR-0057) draw from the same bucket. Exhaustion fails the call
   with `ErrCodeQuotaExceeded` (-8) plus a warn-level log naming the plugin.
   Buckets are created lazily on a plugin's first call, so the map is bounded
   by the installed plugin count; the allowance is **process-local state and
   resets on restart** (acceptable: a restart already resets in-flight rate
   windows for every other limiter-shaped mechanism, and the operator-facing
   semantics is requests/minute sustained, not a daily budget).

2. **Response size cap.** `HostConfig.MaxHTTPResponseBytes` (<= 0 selects
   the default 32 MiB) wraps each successful response body in a counting
   `limitedBody`. The body read that would push delivered bytes past the cap
   fails loudly with `ErrCodeTooLarge` (-11) — never a silent truncation the
   guest could mistake for complete data — and every later read keeps
   failing, so a guest cannot retry-loop around the cap. A warn-level
   security log names the plugin, the target host, and the limit. Status and
   header reads are unaffected: the cap governs body bytes only.

3. **Configuration.** `plugin.http_rate_per_minute` (default 120) and
   `plugin.max_http_response_mb` (default 32) join `internal/config`'s Plugin
   section with non-negative validation; `internal/app` and the `ncgo-cli`
   install host both pass them through (install hooks can call
   `http_request`). Zero explicitly set means the host default — there is no
   "unlimited" setting, matching the default-deny posture. The burst knob
   stays a `HostConfig` field only (same precedent as `MaxSpoolBytes`): a
   testing/embedding control, not an operator key.

4. **Metrics.** No new families. The ADR-0055 wrapper already counts every
   host call with a bounded `result` label: `ErrTooLarge` classifies as
   `too_large` and `ErrCodeQuotaExceeded` as `internal` in
   `ncgo_plugin_host_calls_total`. The `capability_denials` family stays
   scoped to -3: a rate-limit or cap refusal is not a capability denial, and
   the warn logs carry the security signal.

## Alternatives Considered

### Silent truncation at the cap
- Pros: simple guests never see an error.
- Cons: a truncated body is indistinguishable from a complete one — the
  worst failure mode for a plugin consuming JSON/API payloads. Failing
  loudly with -11 makes the contract explicit, and the spec's error table
  already carries `ErrTooLarge`.

### x/time/rate
- Pros: battle-tested limiter.
- Cons: a new dependency for ~40 lines of stdlib; the project's 4i registry
  set the zero-new-dependency precedent for mechanisms this small.

### Distributed/persistent rate state (Redis)
- Pros: consistent limits across replicas and restarts.
- Cons: a network round trip per plugin request, new failure modes, and
  configuration surface; the threat model (runaway plugin) is served fine by
  process-local buckets, and multi-replica deployments can divide the rate
  by replica count.

## Consequences

- ADR-0043's outbound follow-up list is now down to one item: streaming
  request bodies above 1 MiB (an ABI extension — new host function,
  pluginsdk binding, wasmgen probe — tracked as its own increment).
- The cap counts bytes delivered toward the guest; the read that crosses the
  cap consumes its chunk from the transport before failing, so connection
  reuse for a capped-then-closed response is forgone — a fair trade for a
  loud, unbypassable signal.
- Rate state resets on restart, so a plugin gets a fresh burst after every
  boot; documented in the config keys' doc comments.
- Both limits are orthogonal to the ADR-0057 egress guard: the guard decides
  *where* a connection may go, these decide *how often* and *how much* comes
  back. Neither weakens the other.
- Spec §6 documents the semantics; the config keys appear in the Phase 0
  blueprint YAML block.

## Verification

- Token-bucket unit tests: burst-then-deny boundary, refill on a fake clock,
  refill capped at burst after a long idle, per-plugin isolation with lazy
  bucket creation, HostConfig zero-value defaulting.
- Wasm integration (httptest + probes): a 38-byte body against an 8-byte cap
  yields -11 from `http_response_body_read` plus the warn log naming the
  plugin; a body exactly at the cap reads to EOF unimpeded; burst-1 config
  lets the first request complete fully and the second fail with -8 plus the
  warn log; default-config hosts leave the existing outbound suite
  untouched.
- Config: defaults snapshot, full-file parse of both keys, negative-value
  validation for each.
- `go test ./...` green, race clean, golangci-lint 0 issues.
