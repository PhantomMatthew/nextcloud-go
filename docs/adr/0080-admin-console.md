# ADR-0080: Phase 5h embedded admin console

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0054 (resolves its embedded-console follow-up)

## Context

ADR-0054 made serving the compiled Nextcloud frontend strictly opt-in
(`web.static_root`, empty = disabled) and rejected embedding that frontend in
the binary: it would require a Node build pipeline or committed compiled
assets, bloat the binary for headless deployments, and couple server releases
to frontend versions. It explicitly listed a smaller follow-up instead: an
embedded *minimal* admin console (status, users, jobs) via `embed.FS` as a
zero-config alternative to pointing at a full Nextcloud release.

Without it, a default install has no browser surface at all: an operator who
does not supply `web.static_root` gets 404s everywhere a browser could go,
and even `status.php` is a client-facing JSON document, not an admin view.
There is no zero-config way to answer "is the instance up, who has an
account, what is the job runner doing."

A second gap blocked the console's authorization model: Nextcloud's
convention is that members of the `admin` group are instance administrators,
but `EnsureBootstrapAdmin` created the bootstrap admin *user* only — no
group, no membership — so ncgo had no anchor for that convention on fresh
installs. (NC-imported instances already carry the `admin` group and its
memberships via the users importer's group/membership pass.)

## Decision

1. **Hand-written UI embedded via `embed.FS`, no framework, no build step.**
   `internal/console/ui` holds three files (`index.html`, `console.js`,
   `console.css` — vanilla HTML/JS/CSS, ~250 lines total, maintained by
   hand) embedded into the binary. This is the ADR-0054 middle path, not
   the rejected full-frontend embed: no Node toolchain, no vendored compiled
   bundles, no release coupling, and a binary-weight measured in kilobytes.
   The page renders three sections (Status, Users, Jobs) from three
   read-only JSON endpoints; 401s surface a visible message with a link to
   `/index.php/login`, 403s an admin-only notice.

2. **`/console` namespace.** Nextcloud has no such route, so the console
   never shadows or is shadowed by the served frontend — unlike mounting
   under `/index.php/settings/...`, whose paths the NC SPA's own router
   claims. The httpx router's exact/longest-prefix rules keep `/console`
   (exact) and `/console/` (prefix) ahead of the static catch-all `/` mount;
   the app-level test asserts that interplay. Assets are served only by
   exact name (`console.js`, `console.css`) — any other `/console/*` path
   404s; there is no SPA fallback. Everything is `Cache-Control: no-cache`.

3. **Admin-group authorization, NC's convention.** The mount chain is
   `console.Auth(authCfg)` — the same `auth.Middleware` credential stack as
   every browser-facing mount (session cookie, basic, app-password, bearer)
   with a console-shaped failure writer, mirroring the
   `webdav.Auth`/`ocs.Auth` pattern — followed by `console.RequireAdmin`,
   which resolves the principal's group gids and requires `admin`. No
   principal is a 401, a non-admin principal is a 403 (both JSON on
   `/console/api/*`, a tiny login-link HTML page elsewhere — an anonymous
   browser never meets the WebDAV empty-challenge 401), and a failing group
   lookup is a 500: an indeterminate answer never opens the gate.

4. **Bootstrap extension: the `admin` group on the empty-database path
   only.** `EnsureBootstrapAdmin` now also ensures the `admin` group exists
   (`GetGroupByGID`, create on `ErrNotFound`) and the bootstrap admin is a
   member (checked first; a pre-existing membership is not an error).
   Existing installs are untouched — the whole path runs only when the users
   table is empty; NC-imported instances inherit NC's own `admin` group.

5. **Read-only v1.** Every console route is GET/HEAD (anything else → 405
   with `Allow: GET, HEAD`, at both router and handler level). Safe methods
   pass the CSRF chain unchanged and session-authenticated requests need no
   requesttoken, so the console adds zero CSRF surface. Mutations
   (disable-user, run-job, quota edits) are future work and will go through
   the ADR-0064 requesttoken flow like every other unsafe browser request.

6. **Endpoint shapes.** `GET /console/api/status` aggregates ncgo version,
   instance ID, `db {dialect, reachable}` (a real `Ping`), storage
   `{default, backends: [{name, type}]}` from config, the
   encryption/metrics/previews/plugins enabled flags, total user count,
   server time (UTC), and the status.php payload fields (`installed`,
   `maintenance`, `needsDbUpgrade`, version/edition/productname) — the same
   `status.Provider` value the `/status.php` mount uses, hoisted and shared,
   not re-derived. `GET /console/api/users?offset=&limit=` answers
   `{total, users: [{uid, displayname, email, enabled, quota_bytes}]}`
   (`quota_bytes` null = unlimited); per-user groups are deliberately
   omitted in v1 — resolving them is an N+1 against the groups tables, and
   the page does not need them. `GET /console/api/jobs?limit=` answers
   `{jobs: [{id, name, run_at, started_at, completed_at, last_error,
   attempts, state}]}` over a new `jobs.SQLStore.ListRecent` (id DESC, any
   state). `state` derives from the row fields: `done` when completed
   (a retried-then-finished job is done), else `failed` when `last_error`
   is non-empty, else `running` when started, else `queued`. All
   timestamps are RFC3339 UTC; the nullable started/completed columns
   serialize as `null`. Pagination: `limit` defaults to 50 and clamps to
   [1,200], `offset` clamps to ≥ 0, unparsable values fall back to the
   default.

## Alternatives Considered

### Embedding the compiled Nextcloud frontend after all
- Cons: exactly the ADR-0054 rejection — Node toolchain or committed
  build artifacts, binary bloat, release coupling. Still rejected; this ADR
  ships the listed follow-up instead.

### Mounting under `/index.php/settings/admin/...`
- Pros: looks familiar to NC operators.
- Cons: those paths belong to the NC SPA's client-side router; when
  `web.static_root` is enabled the two surfaces would fight over the same
  namespace, and when disabled the familiarity buys nothing. `/console` is
  unclaimed upstream and collision-free by construction.

### Admin flag on the user row (or a config UID list) instead of the group
- Pros: no group lookup per request.
- Cons: diverges from NC's `admin`-group convention; imported instances
  would need a second authorization mapping on top of the one the importer
  already lands, and every future admin-gated surface would reinvent it.

### Baking per-user groups into the users payload
- Cons: N+1 against `group_members` per page; v1's page has no use for
  them. A future version can add a `?groups=1` expansion or a per-user
  detail endpoint without breaking the v1 shape.

## Consequences

- Every install, however configured, now has a working admin view at
  `/console`; operators who never set `web.static_root` get status, users,
  and jobs visibility with zero setup.
- Fresh installs get a real `admin` group with the bootstrap admin in it;
  group-management surfaces (when they arrive) start from a sane state.
- Installs created *before* this change have no `admin` group, so the
  console denies everyone until a membership exists (created via the users
  importer on NC imports, or SQL/CLI by hand). That is the honest failure
  mode: the gate never opens on uncertainty.
- The console is a second, tiny frontend in the repo; the hand-written
  constraint (no framework, no build step, ~250 lines) is what keeps that
  maintainable and is part of the decision, not a style preference.
- ADR-0054's embedded-console follow-up is resolved; its precompressed-
  assets follow-up remains open.

## Verification

- `internal/users/users_test.go`: fresh DB creates the `admin` group with
  the bootstrap admin as member; a second bootstrap on the non-empty table
  adds nothing (no duplicate-membership error); a pre-existing group +
  membership (the NC-imported shape) survives `ensureAdminMembership`
  untouched; a non-empty users table never gains the group.
- `internal/jobs/store_test.go` `TestListRecent`: id DESC ordering, limit
  honored with non-positive default fallback, and all four state shapes
  (queued / running / done / failed) round-trip with their nullable
  timestamps and error text.
- `internal/console/console_test.go`: table-driven httptest coverage —
  anonymous 401 (JSON on api paths, login-link HTML on page/assets),
  non-admin 403, group-lookup-error 500, admin 200 on the page, both
  assets, and all three endpoints; users payload (`quota_bytes` null vs
  set), pagination defaults and clamps, jobs payload (state derivation
  order, RFC3339 timestamps, null started/completed), unknown asset 404,
  POST/PUT 405 with `Allow: GET, HEAD`, HEAD asset with empty body, and
  the `console.Auth` failure writer through the real `auth.Middleware`.
- `internal/app/console_route_test.go`: `/console` mounted ahead of the
  static catch-all (catch-all control still serves the SPA shell),
  anonymous browser gets the login-link page, bootstrap admin passes the
  gate end-to-end over basic auth, non-admin 403, POST 405.
- `go test ./...` green, `go test -race` clean (console, users, jobs,
  app), `golangci-lint run ./...` 0 issues, `gofmt`/`golangci-lint fmt`
  clean, `go mod tidy` no diff (zero new dependencies).
