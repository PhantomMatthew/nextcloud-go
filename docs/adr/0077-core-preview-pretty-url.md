# ADR-0077: Phase 5e extensionless /core/preview mounts (pretty URLs)

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0069 (resolves its extensionless-preview follow-up)

## Context

Phase 4w (ADR-0069) injected `window._oc_config` with
`modRewriteWorking: true` into the SPA shell, matching what a real
Nextcloud reports when pretty URLs are active. With that flag the NC web
frontend's `OC.generateUrl('/core/preview?...')` strips the `/index.php`
prefix, so the UI's own preview `<img>` requests target
`/core/preview` and `/core/preview.png` — routes ncgo never mounted
(only the `/index.php/...` pair existed, `internal/app/routes.go`). The
known consequence, documented as a 4w follow-up, was a 404 for every
UI-generated preview URL.

## Decision

Mount the extensionless pair alongside the existing ones:
`GET /core/preview` and `GET /core/preview.png`, with the same
`webdav.Auth` middleware and the same `preview.Generator` handler
(`internal/app/routes.go`). The handler is path-agnostic — it reads only
query parameters (`file`, `x`, `y`), never the URL path — so both mount
points behave identically and no handler change is needed.

Scope is deliberately limited to the routes the 4w follow-up named. Other
pretty URLs the frontend may generate (apps, avatar, theming) are not
mounted here; they join the plan only when a captured client request
shows a real 404, per the wire-compat evidence rule.

## Alternatives Considered

### Advertising `modRewriteWorking: false` instead
- Pros: no new mounts; the UI would keep using `/index.php/...`.
- Cons: lies about the server to save two route lines; pretty URLs are
  the NC default operators expect, and other consumers of `_oc_config`
  may branch on the flag. The mounts are the honest fix.

### A generic `/index.php`-stripping middleware
- Pros: one shim covers every future pretty URL.
- Cons: silently rewrites the route space, hides which routes are
  actually exercised, and complicates the tracing span names (the router
  route name feeds them, ADR-0072). Explicit mounts keep the route table
  honest.

## Consequences

- UI-generated preview URLs work; no behavior change for the
  `/index.php/...` pair (regression-pinned by test).
- Any future pretty-URL route follows the same pattern: an explicit
  extensionless mount next to its `/index.php` sibling, justified by a
  captured request.

## Verification

- `internal/app/preview_route_test.go` `TestCorePreviewPrettyURL`: all
  four paths (both prefixes × bare and .png) answer 401 unauthenticated
  (mounted and auth-guarded), while an unmounted `/core/...` control
  path does not.
- `go test ./internal/app/` green, `golangci-lint run ./...` 0 issues,
  `gofmt`/`go mod tidy` clean (no new dependencies).
