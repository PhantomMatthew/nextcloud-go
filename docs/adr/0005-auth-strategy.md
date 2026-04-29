# ADR-0005: Authentication Strategy for Phase 1

- Status: Accepted
- Date: 2026-04-29
- Deciders: nextcloud-go core
- Supersedes: —

## Context

Phase 1 needs enough authentication to serve `/status.php` (anonymous), the
OCS capabilities endpoint (anonymous + authenticated variants), and
`/ocs/v1.php/cloud/user` (authenticated). The full upstream auth surface
(SAML, OIDC, WebAuthn, two-factor, login flow v2, app passwords with scoped
tokens, remember-me cookies, brute-force throttling, session pinning) is far
larger than Phase 1 needs. We must pick a minimal slice that keeps the wire
contract intact for desktop/mobile clients.

Upstream references:

- `lib/private/User/Session.php` – orchestrates Basic Auth, token login,
  remember-me, app passwords.
- `lib/private/Authentication/Token/IProvider.php` and `PublicKeyTokenProvider.php`
  – token storage and validation.
- `lib/private/Security/Bruteforce/Throttler.php` – per-IP/action throttling.
- `lib/private/AppFramework/Middleware/Security/SecurityMiddleware.php` –
  CSRF/auth annotations (`@NoCSRFRequired`, `@PublicPage`).
- `OCS-APIRequest` header bypass for CSRF (see ADR-0004).

## Decision

Phase 1 ships a **deliberately narrow** authentication surface:

### In scope (Phase 1)

1. **HTTP Basic Auth** against the user store.
   - Header: `Authorization: Basic <base64(user:password)>`
   - On failure: `WWW-Authenticate: Basic realm="Nextcloud"` + HTTP 401
     (or OCS `RESPOND_UNAUTHORISED` envelope when the route is OCS).
2. **App passwords** (token-based Basic Auth).
   - Same header shape as Basic Auth; password is a long-lived token rather
     than the account password.
   - Validated against the same store used by upstream
     `PublicKeyTokenProvider`. Phase 1 stores tokens hashed with
     `argon2id` (parameters from
     `lib/private/Security/Hasher.php` defaults).
3. **Anonymous access** for `@PublicPage`-equivalent routes.
   - `/status.php`, `/ocs/v{1,2}.php/cloud/capabilities` (subset),
     `/ocs/v{1,2}.php/cloud/user` is **not** anonymous.
4. **Brute-force throttling hook**.
   - Single integration point; default implementation is in-process token
     bucket keyed by `(remoteIP, action)` with `action="login"`.
   - Pluggable later via the WASM plugin ABI (out of scope for Phase 1).
5. **CSRF bypass via `OCS-APIRequest: true`** (see ADR-0004).

### Out of scope (Phase 1, deferred to Phase 2+)

- Login flow v2 (`/index.php/login/v2`)
- OAuth2 client/PKCE flows
- SAML / OIDC / Social login
- Two-factor authentication providers
- Remember-me cookies (`nc_username`, `nc_token`, `nc_session_id`)
- WebAuthn passkeys
- Session pinning across IP changes
- Per-app-password scopes (`filesystem`, `read-only`)

These omissions are visible to clients only as "auth method X not supported";
desktop and mobile clients fall back to Basic Auth + app password, which is
the most common production path.

### Module layout

```
internal/auth/
  basic.go         // HTTP Basic header parsing + WWW-Authenticate emit
  token.go         // app-password validation against user store
  bruteforce.go    // Throttler interface + in-memory default
  middleware.go    // net/http middleware adapter
  user.go          // resolved-user struct passed via request context
  doc.go
```

`internal/auth` exposes a single `Middleware(next http.Handler) http.Handler`
that:

1. Parses `Authorization: Basic`.
2. Tries app-password validation first, then password validation.
3. On success, attaches a `*auth.User` to the request context.
4. On failure, calls the throttler, sets `WWW-Authenticate`, and writes
   either an HTTP 401 or an OCS envelope depending on the route's declared
   transport (a context value set by the router).

### Storage

Phase 1 uses the Phase 0 SQL schema for users and tokens. PostgreSQL,
MySQL/MariaDB, and SQLite are all supported via the same `database/sql`
abstraction.

### Hashing

- Account passwords: `argon2id`, parameters matching upstream
  `Hasher::PASSWORD_DEFAULT_OPTIONS` (memory_cost=64MiB, time_cost=4,
  threads=1) so existing user records remain verifiable.
- App-password tokens: same `argon2id` parameters; tokens themselves are
  generated as 72-char URL-safe random strings to match upstream
  `IToken::TOKEN_LENGTH`.

## Consequences

- Desktop and mobile clients can authenticate against Phase 1 servers using
  app passwords without code changes.
- Federated/social-login clients will see a clean "method not supported"
  failure rather than partial behaviour.
- Brute-force throttling is observable from day one, which keeps Phase 1
  golden cases reproducible (we expose a clock seam for tests).
- Adding flows in Phase 2 (OAuth2, login flow v2) is additive: the
  middleware contract does not change.

## References

- Upstream PHP files listed in Context.
- `docs/plans/03-phase-1-blueprint.md` §4.
- ADR-0004 (OCS envelope, CSRF bypass).
