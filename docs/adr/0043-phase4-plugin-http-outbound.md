# ADR-0043: Phase 4c5 Plugin Outbound HTTP Host Functions

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Spec §6.3 defines the `ncgo.http_*` family (`http_request`,
`http_response_status`, `http_response_header`, `http_response_body_read`,
`http_response_close`) gated by the `http.outbound` capability — a manifest
list of `host[:port]` entries. Phase 4a registered them as stubs returning
`ErrUnsupported` after the capability check. This increment lands the real
implementation: plugins can call external HTTP(S) APIs, but only to hosts
the admin explicitly granted, with response bodies streamed back through a
per-instance handle.

## Decision

1. **Request shape and validation.** `http_request` takes a MessagePack map
   `{method, url, headers, body_bytes, timeout_ms}` (≤ 1 MiB via the shared
   payload cap). Method must be one of GET/HEAD/POST/PUT/DELETE/PATCH/
   OPTIONS (else `ErrInvalidArgument`); the URL must parse with scheme
   `http` or `https` and a non-empty host (else `ErrInvalidArgument`, so
   `file:`, `ftp:`, and scheme-less URLs are rejected before any network
   activity). Malformed MessagePack is `ErrInvalidArgument` too.

2. **Allowlist semantics.** The request target is normalized to
   lowercase `host` when the port is absent or the scheme default (80 for
   http, 443 for https), else `host:port`. Matching against the grant list
   is exact and case-insensitive: a grant `example.com` covers
   `http://example.com/…`, `http://example.com:80/…`, and
   `https://example.com:443/…` but **not** `example.com:8080` and **not**
   `sub.example.com` (no subdomain implication); a grant
   `example.com:8080` matches exactly that host:port. IPv6 literals can
   never match (the manifest host:port grammar forbids colons in hosts).
   No grant → `ErrCodePermissionDenied`.

3. **Redirects are re-validated.** The configured client is shallow-copied
   per request and given a `CheckRedirect` that runs the same allowlist
   check on every redirect target (a 302 to a non-granted host fails the
   whole request with `ErrCodePermissionDenied` via a sentinel error).
   The shared client is never mutated; an admin-supplied `CheckRedirect`
   still runs after the allowlist passes.

4. **Timeout policy.** `timeout_ms` ≤ 0 selects the default 10s; anything
   above 30s is clamped to 30s (never an error). The timeout context
   derives from the call context and lives until the response handle is
   closed, so it also bounds body streaming. Deadline expiry maps to
   `ErrCodeTimeout`, cancellation to `ErrCodeCanceled`, other network
   errors to `ErrCodeInternal` with a warn log.

5. **Host header cannot be spoofed.** Plugin-supplied `Host` headers are
   silently dropped; all other headers are set verbatim. The body is sent
   from `body_bytes` (already bounded by the 1 MiB payload cap).

6. **Streaming responses through handles.** On success the response is
   registered in the caller's per-instance handle table under `handleHTTP`
   (budget 16; `ErrCodeUnavailable` beyond it) wrapped in a type whose
   `Close()` closes the body and cancels the request context, so
   `closeAll` releases leaked responses on every instance-release path.
   `http_response_status` returns the status code;
   `http_response_header` joins multiple values with `", "` (RFC 9110
   list semantics) and writes 0 bytes for an absent header (return 0);
   `http_response_body_read` streams `Body.Read` into guest memory
   (0 = EOF); `http_response_close` closes and removes the handle.
   Unknown/stale handles map to `ErrCodeNotFound`, mirroring abi_db /
   abi_storage. All five functions check `hasHTTPOutbound` first, keeping
   the default-deny posture.

7. **Wiring.** `HostConfig` gains `HTTPClient *http.Client`; nil in
   `NewHost` constructs `&http.Client{Timeout: 30s}`. app.go leaves it
   nil (the app's `UseHTTPClient` client serves OCM/lookup, a different
   trust domain; the plugin host builds its own default). pluginsdk gains
   `HTTPDo` plus `HTTPResponse.Status/Header/Read/Close` bindings and the
   `HTTPOutboundRequest` MessagePack shape, with non-wasm stubs.

## Alternatives Considered

### Wildcard / suffix host grants (`*.example.com`)
- Pros: convenient for APIs spread across subdomains.
- Cons: quietly widens the SSRF surface; `*.example.com` globs are easy
  to over-grant and hard to audit. Exact entries keep the default-deny
  posture; wildcards can be added later as an explicit manifest feature.

### Whole-response buffering instead of streaming handles
- Pros: no handle lifecycle; simpler guest code.
- Cons: unbounded memory in the host for large responses, and the spec
  already defines the handle-based streaming API. The 16-handle budget
  plus closeAll cleanup bounds resource usage.

### Rejecting redirects outright
- Pros: no redirect re-validation logic; smallest attack surface.
- Cons: real APIs redirect routinely (moved endpoints, auth flows);
  re-validating each hop preserves usability without weakening the
  allowlist.

## Consequences

- The allowlist is hostname-based: DNS rebinding and direct-IP grants are
  out of scope for v1. Follow-ups: IP-range SSRF blocking (deny
  RFC1918/loopback/link-local targets unless explicitly granted), DNS
  pinning (resolve once, connect to the resolved IP), per-plugin rate
  limits, and response size caps.
- Request bodies are capped at 1 MiB (shared payload cap); larger uploads
  need a streaming-request follow-up.
- Plugin timeouts compose with the per-call CPU timeout: a slow upstream
  consumes the call budget as usual.
- wasmgen's `opI32GtS` constant was corrected (0x4e → 0x4a; it had been
  emitting `i32.ge_s`, invisible to earlier probes whose checks never
  distinguished 0 from positive).

## Verification

- Capability matrix through wasm guests: no grant → -3, grant for another
  host → -3, exact host:port grant → OK, host-only grant matches a
  default-port URL (via a rewriting RoundTripper), redirect to a granted
  path follows, redirect to a non-granted host:port → -3.
- Validation: `file:`/`ftp:` URLs, unknown method, malformed MessagePack
  → -2.
- End-to-end through wasm guests against httptest servers: GET with
  status/header/body assertions, POST body echo, spoofed `Host` header
  dropped, absent header → 0 bytes, stale handle after close → -4,
  17th open response → -12, leaked response body closed by closeAll
  (counting RoundTripper), upstream slower than `timeout_ms` → -6.
- `canHTTPOutbound`/`httpTarget` table tests: default-port normalization,
  explicit ports, case-insensitivity, no subdomain implication.
- `go test ./...` green, race clean, golangci-lint 0 issues.
