# ADR-0054: Phase 4h static serving for the Nextcloud admin frontend

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Nextcloud's web UI is a pre-compiled Vue frontend: static JS/CSS bundles, an
`index.html` shell, and per-app asset directories. PHP Nextcloud serves these
from its web root and bootstraps the SPA with server-rendered state. ncgo
rewrites the *server*; the frontend is explicitly reused, not rewritten
(README Non-Goals). For the admin UI to be reachable at all, ncgo must serve
those compiled assets and the SPA shell — without taking on a Node build
pipeline, and without pretending to API surface the backend does not have.

Constraints shaping the design:

- **Serve, don't build.** No bundler, no Node toolchain, no vendored assets
  in the repo. The operator supplies a directory of compiled assets (a
  Nextcloud release tarball's web root, or a separately built apps dir).
- **Honesty about API coverage.** The frontend calls login v2
  (`/index.php/login/v2/*`), OCS, and DAV endpoints. Those are implemented
  to the extent covered by Phases 1–3; everything else surfaces as frontend
  errors, not server crashes. ncgo does not implement the `requesttoken`
  flow PHP Nextcloud uses to arm SPA form POSTs — login v2 (client flow)
  and HTTP Basic/app-password are the supported entry points in v1.
- **Static file servers are a classic traversal/escape vector.** Serving an
  operator-provided directory must never leak files outside it.
- **No new third-party dependencies** (project-wide rule).

## Decision

1. **Operator-provided static root, disabled by default.** New config
   `web.static_root` (absolute path, validated; empty = disabled). When set,
   `app.New` constructs `internal/web.StaticUI` via `NewStaticUI`, which
   fails boot loudly if the root is missing, not a directory, or
   unresolvable — a misconfigured UI must never silently degrade to 404s.

2. **Catch-all mounting under existing routes.** The handler mounts as a
   GET/HEAD prefix on `/` in `mountRoutes`. The httpx router matches exact
   routes first, then longest prefix, so every API route (`/ocs/...`,
   `/remote.php/...`, `/index.php/login/v2*`, `/status.php`, plugin routes)
   keeps priority; only paths nothing else claimed reach the static handler.
   Verified by a router-interplay test. HEAD is mounted alongside GET
   because the router does not imply one from the other.

3. **Containment.** URL paths with a `..` segment (raw or percent-decoded;
   Go decodes `%2e` into `URL.Path`) are rejected 404. The remaining path is
   `path.Clean`ed, joined under the root, and prefix-checked against the
   symlink-resolved absolute root; the final file path is
   `filepath.EvalSymlinks`'d per request and re-checked, so a symlink inside
   the root pointing outside also 404s. Tests plant a canary file next to
   the root plus an escaping symlink and prove both unreachable.

4. **No directory listing, no dotfile special-casing.** A directory request
   serves `<dir>/index.html` when present, else 404. (A real directory
   without an index is a *hard* 404 — it is not an SPA-fallback candidate;
   otherwise typo'd asset directories would return HTML with status 200.)

5. **Content types by explicit allowlist.** Extensions map to fixed MIME
   types (.js/.mjs, .css, .html, .json/.map, images, fonts, .webmanifest,
   .txt, .ico); anything else is `application/octet-stream`. Sniffing is not
   used: the allowlist plus the base chain's `X-Content-Type-Options:
   nosniff` (already emitted by `httpx.SecurityHeaders`, not duplicated)
   keeps type confusion out. Bodies stream through `http.ServeContent`,
   which supplies `Last-Modified`, `If-Modified-Since` → 304, Range, and
   HEAD handling for free.

6. **Cache policy keyed on filename shape.** Nextcloud's build bakes a
   content hash into bundle names (`main-1a2b3c4d.js`). Names matching
   `[-.][0-9a-f]{8,}` before the extension get `Cache-Control: public,
   max-age=31536000, immutable`; the SPA shell and everything else get
   `Cache-Control: no-cache` (revalidate, may still serve from cache after
   validation). This mirrors upstream's Apache defaults: hashed bundles are
   safe to cache forever because a rebuild renames them; `index.html` must
   never be stale because it references the bundle names.

7. **SPA fallback for extensionless misses.** A GET/HEAD that matches no
   file *and* whose path has no file extension (`/apps/dashboard`,
   `/index.php/apps/files`) serves `Root/index.html` with 200 — the Vue
   router owns those URLs client-side. A miss *with* an extension
   (`/core/dist/missing.js`) is a real 404: serving HTML where JS/CSS was
   requested would produce confusing parse errors and mask broken deploys.
   Non-GET/HEAD methods get 405 with `Allow: GET, HEAD`.

8. **No CSRF weakening.** Static GET/HEAD are safe methods and pass the
   existing `httpx.CSRF` chain unchanged; no path is added to the CSRF
   bypass list. Browser form POSTs from the SPA that rely on a
   `requesttoken` will fail with 412 in v1 — that is the documented,
   intended consequence of not implementing the token flow, not a bug to be
   papered over by opening CSRF holes.

## Alternatives Considered

### Embedding a built frontend in the binary (`embed.FS`)
- Pros: zero operator setup; single artifact.
- Cons: requires committing compiled assets or a build pipeline to produce
  them (out of scope, no Node toolchain), bloats the binary with assets most
  headless deployments never serve, and couples server releases to frontend
  versions. Rejected for v1; an embedded *minimal* admin console is a listed
  follow-up instead.

### `http.FileServer` / `http.ServeFile` directly
- Pros: stdlib, less code.
- Cons: no control over cache policy per filename, no SPA fallback, its
  redirect behavior (trailing-slash, `index.html` → `./`) is wrong for an
  SPA shell served at arbitrary client routes, and its containment story
  would still need the symlink re-check we implement. The small amount of
  custom logic is worth it; `http.ServeContent` is reused for the wire
  semantics it gets right.

### Reverse-proxying assets to a separate static server
- Pros: offloads serving entirely.
- Cons: another moving part for operators, and the SPA fallback still needs
  to live somewhere next to the API origin to avoid CORS/proxy complexity.
  Rejected; nothing stops an operator from fronting ncgo with a CDN on top
  of what we ship.

## Consequences

- New config surface: `web.static_root` (empty default = feature off; a
  server without assets behaves exactly as before, 404ing unknown paths).
- Unknown extensionless GETs now return the SPA shell (200) instead of 404
  when the feature is enabled — that is the point, but it means typo'd API
  paths under no registered route render HTML. Clients speaking OCS/DAV use
  registered routes and are unaffected.
- POST to an unregistered path now returns 405 (the `/` prefix mount claims
  the path for GET/HEAD) instead of 404. Semantically more correct; no
  client impact.
- The frontend is served, not *supported end-to-end*: views backed by
  unimplemented APIs (settings panels, theming, app management) will show
  frontend errors. This is expected and documented, not hidden.

### Follow-ups (explicit, not in v1)

- **Precompressed assets** (gzip/brotli sidecar files, `Content-Encoding`
  negotiation) — compiled bundles are large; serving `.gz`/`.br` sidecars
  when present is a straight win.
- ~~**`requesttoken` endpoint + CSRF token validation** so the full SPA
  browser login and form POSTs work, not just login v2/Basic.~~ (**resolved
  by ADR-0064**: derived per-session requesttokens, shell injection, browser
  login/logout endpoints, and auth-core CSRF validation.)
- **Embedded minimal admin console** (status, users, jobs) via `embed.FS`
  as a zero-config alternative to pointing at a full Nextcloud release.
- ~~**Server-rendered bootstrap state** (the `oc_appconfig`/`OC` initial
  state PHP injects into `index.html`) to reduce frontend error noise on
  first load; requires templating the shell, deliberately out of v1's
  static-only scope.~~ (**Resolved by ADR-0064 + ADR-0069**: ADR-0064
  injected the requesttoken; ADR-0069 injects the `_oc_webroot` /
  `_oc_config` / `oc_appconfig.core` globals and session `data-user` head
  attributes through the same pipeline. Application-level initial-state
  hidden inputs remain a conditional follow-up, see ADR-0069 §5.)

## Verification

- `internal/web/static_test.go`: fixture tree in `t.TempDir()` with a
  canary file outside the root — index at `/` (200, `text/html`, no-cache,
  `Last-Modified`), hashed bundle (`text/javascript`, immutable max-age),
  SVG/octet-stream content types, SPA fallback for `/apps/dashboard` and
  `/index.php/apps/files`, 404 for missing `.js`/`.png` and `/index.php`,
  404 for a directory without an index, 404 for `/../`, `/%2e%2e/`, and
  `/core/../../../` traversal (canary never served), 404 for a symlink
  escaping the root, 304 on `If-Modified-Since`, HEAD with empty body, 405
  with `Allow: GET, HEAD` for POST/PUT/DELETE, `NewStaticUI` validation
  errors (empty/relative/missing/file root), and router interplay proving
  exact and longer-prefix routes win over the `/` mount.
- `internal/config` tests cover the `web.static_root` default (empty), the
  relative-path validation error, and acceptance of an absolute path.
- `go build ./...`, `go test ./...` green, `go test -race` clean (coverage
  75.8%), `golangci-lint run ./...` 0 issues, `gofmt`/`golangci-lint fmt`
  clean, `go mod tidy` no diff.
