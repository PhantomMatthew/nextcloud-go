# ADR-0057: Phase 4l Plugin HTTP Egress Private-IP Guard (SSRF)

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none) — closes the ADR-0043 "IP-range SSRF blocking" follow-up

## Context

ADR-0043 gated plugin outbound HTTP on a **hostname** allowlist
(`http.outbound = [host[:port]]`), validated on the request and on every
redirect hop. It deliberately left IP-range blocking as a follow-up (spec
§13's "All HTTP outbound URLs resolved + IP-checked" item stayed
unchecked). Two bypasses result:

1. **Literal internal IPs.** A grant for `169.254.169.254:80` (or an
   over-broad grant) reaches the cloud metadata endpoint, and any granted
   `host:port` may itself be an internal address the admin did not
   recognize.
2. **DNS rebinding.** A granted public hostname can resolve to
   `127.0.0.1` or an RFC1918 address (attacker-controlled DNS with a low
   TTL, or simply a legitimately split-horizon name). The allowlist checks
   the hostname string, never the answer.

Checking IPs at URL-validation time would open a TOCTOU window (the
attacker rebinds between our check and the real connect), so the check must
happen on the resolved address at connect time.

## Decision

1. **Enforce at dial time with `net.Dialer.Control`.** The guarded client's
   transport carries a dialer whose `Control` hook receives the already
   resolved `IP:port` before the kernel connect. This covers literal-IP
   URLs and DNS answers at the same point, with no TOCTOU window, and
   applies to every connection the transport opens (initial request and
   redirects). An address the hook cannot parse fails closed.

2. **Block set.** The resolved `netip.Addr` is `.Unmap()`ed first (so
   `::ffff:127.0.0.1` is judged as `127.0.0.1`), then refused when any of
   `IsLoopback()`, `IsPrivate()` (RFC1918 + ULA fc00::/7),
   `IsLinkLocalUnicast()` (169.254/16, fe80::/10 — covers cloud metadata),
   or `IsUnspecified()` holds. **Deliberately NOT blocked:** CGNAT
   100.64.0.0/10 (Tailscale and carrier-grade deployments legitimately
   serve APIs from it; blocking would break real plugin integrations) and
   multicast (no SSRF value over TCP).

3. **Dual clients with pool isolation.** `Control` receives no request
   context, so per-plugin authorization cannot happen inside the hook.
   Instead the Host builds two clients at construction: the configured
   `HostConfig.HTTPClient` as-is ("open") and a shallow copy whose cloned
   transport installs the guarded dialer ("guarded"). `httpRequest` picks
   via the new `httpOutboundAllowPrivate()` capability check. Because the
   two clients sit on **separate transports**, their connection pools are
   disjoint: a connection an authorized plugin pooled to a private target
   is never reused to serve an unauthorized plugin's request.

4. **Grant syntax.** Manifest `[capabilities]` gains
   `http.outbound_allow_private = true` (boolean, same style as
   `jobs.register`). It is additive to `http.outbound`: the hostname
   allowlist still gates every request and redirect hop; the boolean only
   lifts the dial-time IP block. Default (absent) is deny.

5. **Guard installation and escape hatches.** The guarded clone is built
   from `http.DefaultTransport` (comma-ok asserted) when the configured
   client has a nil Transport, or from the configured `*http.Transport`'s
   `Clone()`. `DefaultTransport`'s own dialer is the package-standard
   30s/30s one, so replacing it with the guarded equivalent (same
   timeouts plus `Control`) changes nothing but the check. Two cases
   cannot take the guard and fall back to the configured client unchanged
   (debug log; **the operator client then owns egress policy**):
   - a non-`*http.Transport` RoundTripper (no dial path to hook into);
   - an operator-set `Dial`/`DialContext` (overwriting it would silently
     break the operator's egress customization).
   No production wiring sets either today, so the guard is active by
   default.

6. **Error and signal.** The hook returns a sentinel
   `errEgressPrivateIP`; `mapHTTPErr` maps it (through the `*url.Error` /
   `*net.OpError` wrapping) to `ErrCodePermissionDenied` and emits a
   warn-level security log naming the plugin — a guard refusal means the
   allowlist passed yet the target is internal, which is exactly the SSRF
   signal admins should see.

## Alternatives Considered

### Resolve-then-validate before `client.Do`
- Pros: per-request context available; no transport surgery.
- Cons: TOCTOU — the attacker rebinds between our lookup and the dial.
  Pinning the resolved IP into the dialer re-introduces the complexity of
  Control anyway, minus its coverage of every redirect hop automatically.

### Blocking CGNAT (100.64.0.0/10) too
- Pros: marginally smaller internal surface.
- Cons: breaks Tailscale-net and carrier-NAT integrations that are
  legitimate plugin use cases. Operators who want it blocked can inject a
  custom client/dialer (escape hatch) or simply never grant
  `outbound_allow_private`.

### Single client, guard consulted via context
- Pros: one pool.
- Cons: impossible — `Control` gets no context. Smuggling the plugin
  identity through a custom `DialContext` (which does get ctx) was
  rejected: it breaks connection-pool hygiene (a pooled private-target
  connection could serve a later unauthorized request, since the pool key
  is host:port) and re-implements half of Transport. Two clients give a
  hard pool boundary for free.

## Consequences

- The §13 "All HTTP outbound URLs resolved + IP-checked" checklist item is
  now checked; spec §5 documents the grant, §6 the behavior.
- **Proxy interaction.** With `HTTP(S)_PROXY` set (the default transport
  honors `ProxyFromEnvironment`), the guard sees the *proxy's* address,
  not the target's — the check is vacuous for proxied traffic. Deployments
  fronting plugin egress with a proxy on a private address must inject a
  custom client/dialer (escape hatch) and enforce policy there, typically
  at the proxy itself.
- **IPv6 literals.** The manifest `host:port` grammar forbids colons in
  hosts, so `http://[::1]/…` can never match a grant today; the guard is
  defense-in-depth for the day that grammar relaxes, and it already
  governs IPv6 answers returned for granted hostnames.
- **Operator customization.** An operator-supplied `Dial`/`DialContext`
  disables the guard silently apart from the debug log — acceptable
  because such an operator is explicitly assembling the egress path, and
  the ADR + `HostConfig.HTTPClient` doc comment document the transfer of
  responsibility.
- The webhook-forwarder example gains `http.outbound_allow_private = true`
  (its default `localhost:8080` receiver is a private target); dropping
  the grant while keeping a localhost receiver now fails at dial time with
  -3.
- Existing tests that inject custom RoundTrippers are unaffected by the
  guard by design; the allowlist test suite now runs with
  `outbound_allow_private` so it keeps exercising loopback httptest
  servers.

## Verification

- Predicate table tests: loopback v4/v6, RFC1918 edges (172.15 vs
  172.16/172.31), link-local (169.254.169.254, fe80::1), unspecified
  (0.0.0.0, ::), IPv4-mapped forms (mapped loopback blocked, mapped public
  allowed), and the deliberate allows (100.64.0.1 CGNAT, multicast,
  public v4/v6).
- Dial-hook unit tests: public target passes, loopback/mapped-loopback
  refused with the sentinel, unparseable address fails closed; the
  sentinel survives `*url.Error` wrapping through a real guarded-client
  dial against an httptest server.
- Client-construction tests: custom RoundTripper and custom DialContext
  clients returned unchanged; default and plain-transport clients get a
  guarded clone without mutating the operator's transport; `httpClientFor`
  routes nil/ungranted capabilities to the guarded client and granted ones
  to the configured client.
- Capability tests: nil capabilities deny, both boolean states honored;
  manifest TOML decodes `http.outbound_allow_private = true`.
- Wasm integration (httptest on 127.0.0.1): literal-IP grant without the
  boolean → -3 plus the warn log naming the plugin; same grant with
  `outbound_allow_private` → 200; `localhost:<port>` grant (DNS path)
  without the boolean → -3.
- `go test ./...` green, race clean, golangci-lint 0 issues.
