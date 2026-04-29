# Phase 1 Blueprint: Status, OCS Capabilities, Basic Auth, Routing

- Status: Draft (planning only — no code yet)
- Date: 2026-04-29
- Companion ADRs: 0004 (OCS envelope), 0005 (auth strategy)
- Predecessors: `00-phased-rewrite-plan.md`, `01-phase-0-blueprint.md`,
  `02-golden-harness.md`

## 1. Goal

Phase 1 delivers the smallest end-to-end stack that a Nextcloud desktop or
mobile client will accept as a Nextcloud server **for the discovery and
login probe**. Nothing more. Specifically:

1. `GET /status.php` — anonymous server identity probe.
2. `GET /ocs/v{1,2}.php/cloud/capabilities` — capability advertisement.
3. `GET /ocs/v{1,2}.php/cloud/user` — authenticated identity echo.
4. HTTP Basic + app-password auth surface (per ADR-0005).
5. The `index.php` and `ocs/v1.php` / `ocs/v2.php` entrypoint behaviours
   that real clients depend on (maintenance mode, header set,
   `OCS-APIRequest` CSRF bypass, format negotiation).

Anything beyond these four routes is explicitly out of scope and tracked in
later phases.

## 2. Wire Surface (verified against upstream)

### 2.1 `/status.php`

Source: `status.php` (upstream root). Returns JSON only. No auth. Always
HTTP 200 even in maintenance mode (so clients can detect maintenance
without auth). Body shape, in upstream field order:

```
{
  "installed":      <bool>,
  "maintenance":    <bool>,
  "needsDbUpgrade": <bool>,
  "version":        "<dotted version>",
  "versionstring":  "<human version>",
  "edition":        "",
  "productname":    "Nextcloud",
  "extendedSupport": <bool>
}
```

Phase 1 emits this verbatim. Values come from `internal/observability`
build-time vars plus a `runtime.MaintenanceMode` flag set by config.

### 2.2 `/ocs/v{1,2}.php/cloud/capabilities`

OCS envelope per ADR-0004. Phase 1 ships a **deliberately minimal**
capability set, matching upstream `CoreCapabilities` exactly:

```
"core": {
  "pollinterval":   60,
  "webdav-root":    "remote.php/webdav",
  "reference-api":  true,
  "reference-regex": "<URL_REGEX_NO_MODIFIERS>"
}
```

Additional `core.*` keys advertised by upstream
(`bruteforce`, `mod-rewrite-working`, `versionstring`) come from separate
capability providers and are deferred. The capabilities response also
carries a `version` block sourced from `internal/observability`:

```
"version": {
  "major":           <int>,
  "minor":           <int>,
  "micro":           <int>,
  "string":          "<dotted>",
  "edition":         "",
  "extendedSupport": <bool>
}
```

Anonymous and authenticated requests both succeed in Phase 1; the
capability set does not differ between them yet (it does upstream when
apps register conditional providers — out of scope).

### 2.3 `/ocs/v{1,2}.php/cloud/user`

Authenticated. Returns the resolved user. Field set, mined from
`apps/provisioning_api/lib/Controller/UsersController::getCurrentUser`
and pruned to the Phase 1 user model:

```
"id":           "<uid>",
"display-name": "<display name>",
"email":        "<email or empty>",
"language":     "en"
```

Other fields (`quota`, `groups`, `subadmin`, `phone`, `address`, `website`,
`twitter`, `fediverse`, `organisation`, `role`, `headline`, `biography`,
`profile_enabled`, `pronouns`) are **not** advertised in Phase 1; clients
tolerate their absence.

### 2.4 Maintenance mode

Per `ocs/v1.php` and `ocs/v2.php`: when `maintenance=true` or
`needsDbUpgrade=true`, OCS routes return HTTP 503 with header
`X-Nextcloud-Maintenance-Mode: 1` and an OCS envelope where
`meta.statuscode=503` and `meta.message="Service unavailable"`. Phase 1
mirrors this exactly. `/status.php` itself remains HTTP 200 during
maintenance — this is non-obvious and is captured as a golden case.

## 3. Module Layout

```
cmd/
  ncgo-server/
    main.go              // wires config + observability + httpx server
internal/
  config/                // Phase 0 — already scaffolded
  observability/         // Phase 0 — already scaffolded
  httpx/
    server.go            // *http.Server lifecycle, graceful shutdown
    middleware.go        // chain: recover, requestid, logging, security,
                         //        maintenance, csrf, auth (per route)
    security_headers.go  // X-Content-Type-Options, X-Frame-Options,
                         //        Strict-Transport-Security (configurable),
                         //        Referrer-Policy
    maintenance.go       // 503 + X-Nextcloud-Maintenance-Mode for OCS,
                         //        passthrough for /status.php
    csrf.go              // OCS-APIRequest bypass + same-origin form check
    routes.go            // route table; tags routes with transport=ocs|http
    errors.go            // ProblemDetails + OCS error coercion
    doc.go
  ocs/
    envelope.go          // Render(version, format, payload, message)
    statuscode.go        // Map(version, ocsCode) -> httpCode (ADR-0004)
    coerce.go            // exception/error -> envelope
    format.go            // negotiate JSON vs XML (?format / Accept)
    json.go              // JSON encoder (numeric+string preserving)
    xml.go               // XML encoder matching upstream tag layout
    doc.go
  auth/
    basic.go             // Authorization: Basic parsing + WWW-Authenticate
    token.go             // app-password validation
    bruteforce.go        // Throttler interface + in-memory default
    middleware.go        // net/http middleware adapter
    user.go              // resolved-user struct in request context
    doc.go
  capabilities/
    registry.go          // Provider interface + Phase 1 core provider
    core.go              // CoreCapabilities equivalent
    version.go           // version block
    doc.go
  status/
    handler.go           // /status.php JSON
    doc.go
  users/
    handler.go           // /cloud/user
    store.go             // user lookup against SQL store from Phase 0
    doc.go
```

`cmd/ncgo-server/main.go` is the only binary shipped in Phase 1. Other
Phase 0 stubs (`ncgo-migrate`, `ncgo-occ`) remain stubs.

### 3.1 Middleware order (top to bottom)

1. `recover` — convert panics to 500 with request id.
2. `requestid` — assign and propagate `X-Request-Id`.
3. `logging` — structured access log (slog).
4. `security_headers` — fixed response headers.
5. `maintenance` — short-circuits OCS routes to 503; passes
   `/status.php` through.
6. `csrf` — only triggers for state-changing methods. Bypassed by
   `OCS-APIRequest: true` (ADR-0004) or `@PublicPage`-equivalent route tag.
7. `auth` — only triggers for routes tagged `requiresAuth=true`.

The chain is composed once at startup. Each middleware is unit-testable
against `httptest.ResponseRecorder` plus the golden harness for end-to-end
byte parity.

## 4. Routing

Phase 1 uses Go 1.22+ `net/http.ServeMux` pattern matching — no third-party
router. Route table:

| Method | Pattern                                    | Handler              | Auth | Transport |
|--------|--------------------------------------------|----------------------|------|-----------|
| GET    | `/status.php`                              | status.Handler       | no   | http      |
| GET    | `/ocs/v1.php/cloud/capabilities`           | capabilities.GetV1   | opt  | ocs/v1    |
| GET    | `/ocs/v2.php/cloud/capabilities`           | capabilities.GetV2   | opt  | ocs/v2    |
| GET    | `/ocs/v1.php/cloud/user`                   | users.GetSelfV1      | yes  | ocs/v1    |
| GET    | `/ocs/v2.php/cloud/user`                   | users.GetSelfV2      | yes  | ocs/v2    |

Notes:

- The `auth` middleware reads the `Transport` tag from the route to decide
  whether 401 is rendered as a plain HTTP response or as an OCS envelope.
- Trailing slashes are accepted exactly when upstream accepts them
  (golden cases enforce).
- OCS V1 and V2 differ only in status mapping (ADR-0004), not route shape.
  We keep them as separate handlers solely so route tagging is explicit;
  internally both delegate to the same business function.

## 5. Configuration Surface

`internal/config` (Phase 0) gains:

```
[server]
listen_addr = ":8080"
trusted_proxies = []
behind_tls_terminator = false

[server.tls]
enabled = false
cert_file = ""
key_file = ""

[security]
hsts_max_age = 15552000  # 6 months
hsts_include_subdomains = true
hsts_preload = false

[auth]
basic_realm = "Nextcloud"
bruteforce_window = "15m"
bruteforce_threshold = 10

[capabilities]
poll_interval = 60
webdav_root = "remote.php/webdav"

[maintenance]
enabled = false
needs_db_upgrade = false
```

All keys have sane defaults; the file is optional. Maintenance flags are
also settable via `NCGO_MAINTENANCE=1` to allow operators to flip
quickly without editing config.

## 6. Error Handling

Two response formats coexist:

1. **Plain HTTP** for `/status.php` and any non-OCS surface added later.
   Errors render as RFC 7807 Problem Details
   (`Content-Type: application/problem+json`).
2. **OCS envelope** for everything under `/ocs/`. ADR-0004 mapping
   applies. `internal/ocs.Coerce(err)` is the single funnel.

`internal/httpx/errors.go` provides:

```
type Error struct {
    HTTPStatus int
    OCSStatus  int   // 0 means "use HTTP only"
    Code       string
    Message    string
    Cause      error
}
```

The middleware decides which renderer to use based on the route's
transport tag. Handlers never write status codes directly; they return
`*Error` or a typed payload.

## 7. Observability

- Access log: one line per request, slog JSON, fields:
  `ts, level, msg, method, path, status, dur_ms, bytes, ua, request_id,
  user (if authenticated), client_ip`.
- Metrics (Prometheus, optional): `http_requests_total`,
  `http_request_duration_seconds`, `http_responses_bytes`. Not wired to a
  scrape endpoint in Phase 1; metric surface is set up but disabled by
  default.
- Tracing: deferred to Phase 2.

## 8. Test Strategy

### 8.1 Unit tests

- `internal/ocs`: envelope round-trip JSON+XML against fixtures; status
  mapping table per ADR-0004 driven by table-driven tests for both V1 and
  V2; format negotiation.
- `internal/auth`: header parsing edge cases (missing, malformed, base64
  garbage, empty user, empty password, unicode); throttle clock seam.
- `internal/httpx`: middleware order verified by composition tests;
  recover; CSRF bypass; maintenance toggling.

### 8.2 Golden tests

Phase 1 ships these new golden cases (additive on top of Phase 0's
`status/001-status-php-anonymous`):

1. `status/002-status-php-maintenance` — `maintenance=true`, still 200,
   payload reflects flag.
2. `capabilities/001-anonymous-v1` — XML default.
3. `capabilities/002-anonymous-v1-json` — `?format=json`.
4. `capabilities/003-anonymous-v2-json` — V2 envelope, `?format=json`.
5. `capabilities/004-authenticated-v2-json` — same body, with Basic auth.
6. `cloud-user/001-self-v1-json` — happy path.
7. `cloud-user/002-self-v2-json` — happy path V2.
8. `cloud-user/003-unauthenticated-v1` — OCS 997 / HTTP 401, V1
   `WWW-Authenticate` header.
9. `cloud-user/004-unauthenticated-v2` — OCS 997 / HTTP 401, V2 envelope.
10. `cloud-user/005-bad-password-v2` — confirms throttling header is
    absent on first failure (or present with documented threshold).
11. `ocs/001-maintenance-503-v1-xml` — OCS 503 envelope + maintenance
    header.
12. `ocs/002-maintenance-503-v2-json` — same, V2.

All hand-authored cases are tagged `synthetic: true` until the capture
sprint replaces them with replayed mitmproxy traces. Cases 1, 2, 3, 6, 11
must be replayable from a mitmproxy capture before Phase 1 exit.

### 8.3 Integration test (single)

A `cmd/ncgo-server` smoke test boots the binary on `:0`, hits all five
routes, and verifies:

- `/status.php` returns expected JSON shape.
- `/ocs/v1.php/cloud/capabilities` returns XML by default.
- `?format=json` flips encoding.
- `Authorization: Basic` enables `/cloud/user`.
- Toggling `NCGO_MAINTENANCE=1` flips OCS routes to 503 while
  `/status.php` stays 200.

## 9. Acceptance Criteria (Phase 1 exit)

A Phase 1 build is "done" when **all** of these hold:

1. `cmd/ncgo-server` boots from a default config and serves the five
   routes above.
2. Twelve golden cases above are committed and pass under
   `go test ./internal/goldentest/...`.
3. ≥ 5 of the 12 cases are replayable (not `synthetic`).
4. Official Nextcloud desktop client reaches the "enter credentials"
   stage when pointed at the server. (Manual smoke; recorded as a video in
   `docs/evidence/phase1/`.)
5. `go test -race ./...` clean.
6. `golangci-lint run` clean (config from Phase 0).
7. `govulncheck ./...` clean.
8. CI matrix (ubuntu/macos × go 1.22/1.23) green.
9. Coverage ≥ 65% for `internal/ocs`, `internal/auth`, `internal/httpx`.

## 10. Sequencing

Suggested ordering for the implementation sprint (each step ends in a
green commit):

1. `internal/ocs` envelope + status mapping + format negotiation, with
   unit tests and the four `capabilities/*` golden cases stubbed against a
   fake handler.
2. `internal/httpx` middleware chain (recover, requestid, logging,
   security headers, maintenance), wired against a trivial mux.
3. `/status.php` handler — golden cases 1 and 2 go green.
4. `internal/capabilities` provider + handler — golden cases 3, 4, 5, 6
   go green.
5. `internal/auth` Basic + bruteforce + middleware. Hook into router.
   Cases 8, 9, 10 go green.
6. `internal/users` handler. Cases 7, 8 go green.
7. Maintenance OCS short-circuit. Cases 11, 12 go green.
8. `cmd/ncgo-server` end-to-end smoke test.
9. Phase 1 retro and Phase 2 kickoff.

Each step is independently revertable. Steps 1 and 2 can proceed in
parallel by two engineers; everything after step 2 is mostly serial
because handlers depend on the middleware chain.

## 11. Risks and Mitigations

- **Wire drift on XML encoding.** Go's `encoding/xml` differs from PHP's
  `SimpleXMLElement` in attribute ordering and self-closing tag emission.
  Mitigation: golden cases pin bytes; `internal/ocs/xml.go` uses a custom
  encoder that matches PHP's order. A normalizer pass exists for
  whitespace but **not** for tag layout.
- **Capability set incompleteness.** Real clients may probe for keys we
  don't advertise. Mitigation: capture sprint records a real desktop
  client login flow and we extend the capability set to match before Phase
  1 exit.
- **App-password storage compatibility.** We're not migrating from PHP;
  Phase 1 generates fresh tokens. Mitigation: documented in ADR-0005;
  migration is Phase 4.
- **Maintenance flag race.** Toggling at runtime must not break in-flight
  requests. Mitigation: read the flag once per request via
  `atomic.Bool`.

## 12. Out of Scope (Phase 1)

- WebDAV/CalDAV/CardDAV
- File uploads/downloads, chunked uploads, ETag handling
- Sharing API, federation, notifications
- Login flow v2, OAuth2, SAML, OIDC, social login, two-factor
- Theming, branding, mobile push, encryption
- Notify-push WebSocket
- Apps marketplace, plugin loading (Phase 3)
- Migration from upstream PHP installs (Phase 4)

These are tracked in `docs/plans/00-phased-rewrite-plan.md`.

## 13. Open Questions

1. Do we expose the `version` block in capabilities exactly as upstream,
   or is `versionstring` enough for clients we care about? Capture sprint
   will confirm.
2. Bruteforce throttling: should the first failed login already include
   a `OCS-Retry-After`-style header, or only after threshold? Upstream
   `Throttler` behaviour is action-based; we will mirror exactly once
   captured.
3. `mod-rewrite-working` capability — clients use it to choose between
   `/index.php/...` and `/...` URLs. Phase 1 defaults to `false` (URLs
   include `index.php`), pending confirmation that desktop clients accept
   that.

These are tracked separately in the capture sprint backlog; none block
Phase 1 *planning*.
