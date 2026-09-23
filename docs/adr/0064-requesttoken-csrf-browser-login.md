# ADR-0064: requesttoken, CSRF validation, and SPA browser login

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0054 (static-serving follow-ups: requesttoken + CSRF and
  the token part of bootstrap injection are now done)

## Context

ADR-0054 shipped static serving of the compiled Nextcloud web UI but
explicitly left the `requesttoken` flow unimplemented: the SPA shell was
served byte-identically with no bootstrap injection, and ncgo had no
`POST /index.php/login` browser endpoint (only the login v2 client flow).
Consequently SPA browser login and browser form POSTs did not work at all —
the base chain's `httpx.CSRF` blanket answered every session-less unsafe
request with 412.

What the frontend actually needs, verified against upstream sources:

- `core/templates/layout.user.php` and `core/templates/layout.guest.php`
  (the login page) inject the token as a `<head data-requesttoken="...">`
  attribute ([layout.user.php](https://github.com/nextcloud/server/blob/master/core/templates/layout.user.php),
  [layout.guest.php](https://github.com/nextcloud/server/blob/master/core/templates/layout.guest.php)).
- Legacy `core/js/js.js` derives the `window.oc_requesttoken` global from
  that attribute ([stable13 js.js](https://github.com/nextcloud/server/blob/stable13/core/js/js.js));
  `core/src/OC/requesttoken.js` reads the same attribute
  ([stable28 requesttoken.js](https://github.com/nextcloud/server/blob/stable28/core/src/OC/requesttoken.js));
  the modern `@nextcloud/auth` `getRequestToken()` reads
  `document.head.dataset.requesttoken`
  ([nextcloud-auth requestToken.ts](https://github.com/nextcloud-libraries/nextcloud-auth/blob/main/lib/requestToken.ts)).
- The login form POSTs `user`, `password`, `requesttoken` to
  `/index.php/login`; logout is a GET/POST to `/index.php/logout` carrying
  the token in the query or header.

Constraints: no schema change (sessions table exists, no token table), no new
config keys (`instance.secret` is the HMAC root), no new dependencies, and
the existing brute-force throttling (`auth.CacheThrottler`) must cover the
browser login endpoint.

## Decision

1. **Stateless per-session tokens, zero schema change.** The CSRF token is
   derived, never stored:
   `base64url(HMAC-SHA256(instance.secret, "ncgo-requesttoken:" || sessionID))`
   (`internal/auth/requesttoken.go`). Verification recomputes and compares
   decoded MACs with `hmac.Equal` (constant time). The session ID is already
   a 256-bit random value and the cookie already transports it, so the HMAC
   of it is unguessable and bound to exactly one session; rotating or
   deleting the session invalidates the token for free. The anonymous login
   page needs a token before any session exists, so it uses a double-submit
   design in a separate HMAC domain: the shell injection issues a
   `ncgo_login_nonce` cookie (32 random bytes, hex; HttpOnly, SameSite=Lax,
   Secure follows the scheme) when absent, and the injected token is
   `base64url(HMAC-SHA256(secret, "ncgo-login:" || nonce))`. Domain prefixes
   make the two token kinds non-interchangeable.

2. **Shell injection at the two upstream read paths.** When `StaticUI`
   serves the SPA shell — the root `index.html` however reached, and SPA
   fallback responses — it injects both the `<head data-requesttoken="...">`
   attribute (the canonical path read by core `requesttoken.js` and
   `@nextcloud/auth`) and a `<script>window.oc_requesttoken="...";</script>`
   global (legacy path for older bundles), replacing a stale
   `data-requesttoken` attribute if the operator-supplied shell already has
   one. With a valid session cookie the injected token is the session token;
   otherwise it is the login token (issuing the nonce cookie first). Tokens
   are base64url, safe in an HTML attribute and a JS string literal without
   escaping. All other assets stay byte-identical.

3. **Browser login/logout endpoints** (`internal/web/login.go`).
   `POST /index.php/login` validates the login token against the nonce cookie
   *first* (403 on mismatch — there is no session yet, so the auth middleware
   cannot do this), then verifies credentials through the same chain verifier
   login v2 uses. Credential failures are observed by the shared
   `CacheThrottler` (action `login`, same bucket as the middleware) and
   answered 401 after the throttle delay. Success creates the session with
   the same TTL (24 h default), User-Agent, and IP extraction as the login v2
   grant, expires the nonce cookie (rotation), sets `nc_session_id`
   (HttpOnly, SameSite=Lax, Secure when the request is TLS or
   `X-Forwarded-Proto: https`), and 303s to `/`.
   `GET|POST /index.php/logout` requires the session token (header, else
   query — upstream's logout link is a plain GET), deletes the session,
   clears the cookie, and 303s to `/index.php/login`; without a session
   cookie it redirects idempotently. `GET /index.php/login` is explicitly
   mounted to the static handler because the router 405s method mismatches
   on exact routes — without it the login *page* would 405.

4. **CSRF validation lives in the auth middleware's session branch**
   (`internal/auth/middleware.go`). When the request authenticated via
   session cookie and the method is not in the safe set
   (GET/HEAD/OPTIONS/PROPFIND/REPORT — the two WebDAV verbs are read-only),
   the request must echo the session's token in the `requesttoken` header
   (preferred) or a urlencoded form field. The form fallback reads and
   *restores* the body (`io.MultiReader`), because downstream handlers such
   as the sharing OCS endpoint read `r.Body` directly — `r.FormValue` would
   drain it. Failure is a 403 with no fallthrough: the session is valid, the
   request is not. Basic, app-password, bearer, and public-link token
   authentication are inherently exempt — they are not ambient credentials a
   cross-site browser request can attach. Every route family flows through
   this one core: DAV mounts and the preview handler via `webdav.Auth`,
   OCS/activity/notifications/sharing/search via `ocs.Auth`, and plugin
   routes via the reconciler's copies of both.

5. **Base-chain deferral, not removal.** `httpx.CSRF` gains
   `SessionCookie`: an unsafe request carrying `nc_session_id` passes the
   base chain so the auth core can answer 403 (rather than the blanket 412,
   which would shadow it). Anonymous unsafe requests keep the 412 behavior;
   the login v2 flow, `/index.php/login`, `/index.php/logout`, DAV paths,
   public DAV, and OCM stay path-bypassed (the login/logout handlers
   self-validate their tokens).

6. **Injected shells are never 304.** The injected page varies per session,
   so conditional-request negotiation would be incorrect: `serveShell` emits
   no `Last-Modified`, ignores `If-Modified-Since`, and always answers 200
   with the existing `Cache-Control: no-cache`. Assets keep their ADR-0054
   semantics (immutable hashed bundles, revalidation for the rest).

## Alternatives Considered

### Server-side token storage (sessions table column or token table)
- Pros: tokens revocable independent of the session; upstream-shaped.
- Cons: schema change and a store interface change for a value that is
  derivable; the session ID is already a high-entropy per-session secret
  known to the client. Rejected — derivation gives the same binding with no
  storage.

### Validate in the base chain (`httpx.CSRF.Validate` hook) only
- Pros: one layer.
- Cons: the base chain runs before route auth, so the 403/401 distinction
  between "bad token on a valid session" and "no credentials" disappears,
  and plugin routes mounted later would need the same hook wiring anyway.
  The auth-core check is the authoritative one; the base chain keeps its
  anonymous blanket.

### CSRF-exempting `OCS-APIRequest: true` requests in the auth core
- Pros: mirrors upstream's exemption for that header.
- Cons: the header is trivially settable by any browser script, so exempting
  it under *session* auth would nullify the check; upstream gets away with it
  because its session middleware still validates the requesttoken for web
  sessions. We do not exempt it.

## Consequences

- Browser login, SPA form POSTs, and logout work end-to-end against a served
  Nextcloud frontend; desktop/mobile clients (basic/app-password/bearer) are
  untouched.
- `GET /index.php/login` now serves the login shell (200) instead of the SPA
  fallback doing so implicitly — same body, explicit route.
- Unsafe anonymous requests still 412; unsafe session requests without a
  valid token now 403 (previously 412 from the blanket). Browser clients
  always send the token, so this only affects hand-rolled session clients.
- An attacker with a stolen session ID already owns the session; the token
  adds nothing there and is not meant to. It only binds unsafe requests to
  same-origin page loads.
- ADR-0054 follow-up status: **`requesttoken` endpoint + CSRF validation —
  done here**; **server-rendered bootstrap state — partially** (the
  requesttoken is injected; the full `oc_appconfig`/initial-state payload
  remains a follow-up); ~~**precompressed assets**~~ (**resolved by
  ADR-0081**); ~~**embedded minimal admin console**~~ (**resolved by ADR-0080**).

## Verification

- `internal/auth/requesttoken_test.go`: derivation determinism, base64url
  alphabet, tamper/other-session/other-secret/wrong-domain rejection, empty
  and nil inputs, nonce uniqueness.
- `internal/auth/middleware_test.go`: session POST without/with wrong token
  → 403 (next never runs), header token passes, form-field token passes with
  the body provably intact downstream, safe methods (incl. PROPFIND/REPORT)
  skip, basic auth exempt, nil `RequestToken` disables.
- `internal/httpx/csrf_test.go`: session-cookie deferral, empty-cookie and
  anonymous requests still 412.
- `internal/web/static_test.go`: injection at `/`, `/index.html`, and SPA
  fallback; stale attribute replacement; headless-fragment prepend; assets
  byte-identical with 304 semantics intact under an armed injector; injected
  shell never 304s and omits `Last-Modified`; HEAD shell has no body.
- `internal/web/login_test.go`: full HTTP-level chain over a real session
  SQLStore and fixture tree — anonymous shell issues nonce + login token;
  wrong token 403; wrong password 401 with throttling; success 303 with
  session cookie and rotated nonce; authenticated shell carries the session
  token; session POST 403 without / 204 with header or form token; basic POST
  exempt; logout (query and header shapes) deletes the session and 303s;
  deleted session no longer authenticates; logout with a bad token 403s and
  preserves the session; anonymous logout is an idempotent 303.
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...` green,
  `golangci-lint run ./...` 0 issues, `go mod tidy` no-op, `go test -race
  -coverprofile` green (total coverage 76.8%, was 76.7%).
