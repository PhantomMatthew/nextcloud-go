# ADR-0106: WOPI host core (Collabora Online editing, server side)

- **Status**: Accepted
- **Date**: 2026-10-01
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: (none)

## Context

The v2 office epic's first increment lands the **WOPI host core**: the
server side of Collabora Online / OnlyOffice document editing. Upstream
Nextcloud's richdocuments app exposes a session-authed token mint endpoint
and a set of token-authed WOPI callbacks under
`/index.php/apps/richdocuments/wopi/files/{id}[/contents]`; this ADR keeps
exact path parity with that surface so Collabora can integrate
unmodified. The increment deliberately excludes the viewer HTML page, the
Collabora discovery fetch, the richdocuments capabilities block, and any
token-bound key wrap — all are follow-ups (see Consequences).

Verified facts that shaped the build:

- **Bearer-token precedent is plaintext at rest.** login_flows
  (poll/login/state tokens) and public-share tokens are stored verbatim;
  these are machine-generated random credentials, never user-typed
  secrets, so hashing buys nothing an attacker with DB read does not
  already have.
- **The app wires `DAV.Meta` as the TranslatingStore wrapper** (ADR-0104),
  whose `GetByID` decrypts in the request ctx. That is precisely the
  boundary this design needs: mint runs in the caller's **session** ctx
  (an enrolled ADR-0100 user's unlocked key is present), while the WOPI
  callbacks run in an **anonymous** ctx (Collabora holds no session), so
  an enrolled password-wrapped user mints fine and then hits
  `ErrKeyLocked` → 403 on the callbacks — the documented ADR-0101
  boundary, until the token-bound key-wrap follow-up mirrors ADR-0102's
  app-token wraps.
- **`dav.Lock` mints `opaquelocktoken:` ids and has no injection seam.**
  WOPI lock ids are client-chosen arbitrary strings echoed on every
  subsequent callback, so the existing lock verbs cannot serve Collabora.
- **The jobs runner seeds periodic jobs from a fixed name list** at
  `Start`; a new periodic GC must join that list (same rule the preview
  GC comment documents).
- **The CSRF middleware prefix-matches `PathBypass` entries ending in
  `/`.** Collabora's PUT/POST callbacks carry no session cookie and no
  OCS/Bearer header, so the files/ subtree needs the bypass; the GET mint
  endpoint is a safe method and needs none.

## Decision

### 1. Config section `office` (template: `previews`)

`office.enabled` (default false), `office.collabora_url` (default ""),
`office.token_ttl` (default 10h). Validation runs only when enabled
(dead-key rule, same as `previews.office_*`): `collabora_url` must parse
as an absolute http(s) URL with a non-empty host — the config package's
first URL-shape check, no precedent existed — and `token_ttl` must sit in
[1m, 24h].

### 2. Migration 0025 `wopi_tokens` (three dialects in lockstep)

`token TEXT PRIMARY KEY, uid TEXT NOT NULL, file_id <int64>,
can_write <bool> NOT NULL DEFAULT 0/FALSE, expires_at <int64 ms> NOT
NULL`, plus an index on `expires_at` for the GC sweep. Column types
mirror each dialect's login_flows/file_locks conventions (sqlite INTEGER,
postgres BIGINT/BOOLEAN, mysql BIGINT/BOOLEAN + VARCHAR PKs). No FK
clause, matching the explicit-lifecycle convention; the token row is a
grant, not a tree edge — deleting a file or user does not cascade, and
the callbacks' resolve step collapses a dangling row to the same 404/401
as any other miss. Down drops the table.

### 3. `internal/wopi`: token store, service, two handlers, GC job

- **Store**: plaintext bearer tokens with SQL-side expiry filtering
  (`expires_at > now` — a missing and an expired token are
  indistinguishable, one `ErrTokenNotFound`).
- **Service.resolve** maps (uid, fileID) to DAV coordinates through the
  app-wired TranslatingStore `Meta` (NEVER the raw store — the decrypting
  wrapper is what makes the anonymous-callback 403 boundary automatic):
  the owner resolves to her own path with read+update; anyone else
  resolves through `Shares.ListIncoming` (structural `IncomingLister`
  seam — wopi must not import sharing), mapping the owner path to the
  sharee-side mount path. A miss is one undifferentiated not-found: the
  WOPI surface exposes no existence oracle. Directories are rejected
  (mint 400, callbacks 404).
- **Mint** runs in the caller's session ctx (see Context), bakes
  `can_write = perms&PermUpdate != 0` into the token row, and returns the
  WOPI source URL built from the request (mirroring login v2's
  `defaultBaseURL`: request scheme with the trusted-proxy
  X-Forwarded-Proto/Host overrides). Token strings are 32 crypto/rand
  bytes hex-encoded (the `NewToken` seam covers tests).
- **FilesHandler** mounts with NO session middleware: the `access_token`
  query param is the only credential, validated (existence, expiry, and
  binding to the URL's file id) BEFORE any file resolution, so a bad
  token learns nothing — not even whether the id exists. It serves
  CheckFileInfo (minimal field set), GetFile, PutFile (write grant +
  X-WOPI-Lock handshake → 409 JSON on conflict), and the
  X-WOPI-Override ops LOCK / UNLOCK / REFRESH_LOCK (idempotent re-LOCK,
  conflict-mapped mismatches, unknown override → 501). Error mapping:
  not-found → 404, `ErrKeyLocked` (dual-matched via files' keyLockedError)
  → 403, invalid token → 401, lock conflicts → 409.
- **GC**: `wopi.tokens.gc` joins the runner's periodic set, registered
  before `jr.Start` per the preview-GC ordering rule.

### 4. files package lock-token seams (exported, on *DAV)

`LockWithToken` / `UnlockWithToken` / `LockTokenAt` mirror dav.Lock /
dav.Unlock — same LockStore, same timeout constants (1800s default,
86400s cap), same expire-on-read — but store and compare the given token
verbatim (no `opaquelocktoken:` minting, no angle-bracket stripping).
They also mirror CheckLock's incoming-share dispatch, so a sharee's WOPI
lock lands in the OWNER's lock namespace and owner/sharee contend on one
row. Ciphertext mounts need no dedicated branch: the owned path resolves
through the translating lock store keyed by owner id, so an enrolled
owner surfaces `ErrKeyLocked` → 403 in the anonymous callback ctx,
exactly like the content path. Unlock mismatches return
`webdav.ErrConflict`, mirroring dav.Unlock — the handler maps it to 409.

### 5. Routes and wiring

Both mounts exist only when `office.enabled` is set (the
`if a.wopiSvc != nil` gate, mirroring the activity store gate): GET mint
behind `webdav.Auth`, the callbacks prefix-mounted without middleware.
`/index.php/apps/richdocuments/wopi/files/` joins the CSRF PathBypass
(prefix-matched) so Collabora's cookie-less POSTs reach the handler.

### Alternatives considered

- **Hash the tokens at rest** — rejected: the token is a
  machine-generated ≥256-bit bearer credential, not a user-typed secret;
  login_flows and public shares set the plaintext precedent, and an
  attacker reading the DB already owns the files the tokens protect.
- **Reuse dav.Lock's token generation** — rejected: WOPI lock ids are
  client-chosen and must round-trip verbatim through X-WOPI-Lock headers;
  minted `opaquelocktoken:` ids cannot serve the protocol.
- **Resolve callbacks through the raw filecache store and special-case
  decryption** — rejected: the TranslatingStore wrapper already IS the
  decryption boundary every other consumer uses; bypassing it would
  duplicate the ErrKeyLocked mapping and risk leaking ciphertext paths.

## Consequences

**Positive.** Collabora Online can open, lock, read, and write documents
against upstream-path-identical WOPI endpoints; locks are enforced with
client-chosen ids; sharees get read-only WOPI access through their
mounts with no share-specific code in the handler; version snapshots
fire on PutFile through the normal DAV write path.

**Negative.** WOPI tokens are plaintext at rest (accepted, §
Alternatives). A minted token outlives a permission change (revoke /
unshare) until expiry — the TTL bounds the window; per-request re-checks
are a possible hardening follow-up.

**Neutral / follow-ups.** Enrolled password-wrapped users hit the
documented ErrKeyLocked → 403 boundary on the anonymous callbacks; the
token-bound key wrap (mirroring ADR-0102's app-token wraps) closes that.
（**landed 2026-10-01**: viewer page + discovery fetch）The viewer HTML page
(`/index.php/apps/richdocuments/index`, upstream path parity, session-authed)
mints a token, resolves the editor URL from the cached Collabora discovery
document (`{collabora_url}/hosting/discovery`, parsed with stdlib
`encoding/xml`, refreshed at most hourly, edit vs view action chosen by the
token's write grant), and renders an embedded form-post bootstrap into the
Collabora iframe — no Nextcloud credential ever reaches the Collabora
session, and the filename is html/template-escaped. Discovery outages map
to 502, an uneditable extension to 404. Still open for the next epic
increments: the richdocuments capabilities block and additional WOPI
operations (PutRelativeFile, RenameFile). （**landed 2026-10-02**:
capabilities block）The `richdocuments` capability block is registered
whenever the WOPI host is enabled: `version`, the curated common Collabora
`mimetypes` set (ODF + OOXML + legacy Office) with `application/pdf` under
`mimetypesNoDefaultOpen`, `productName`, `templates`/`direct_editing`
false. The capabilities surface is synchronous (no ctx), so it cannot
consult the discovery document — the curated list advertises the common
set while the viewer stays the per-file source of truth (404 when
discovery offers no action).

## Verification

- `internal/wopi` store units (insert/get, expiry filtered, GC count)
  and httptest e2e covering mint, CheckFileInfo field-exactness,
  GetFile byte-exactness, the LOCK/PutFile/UNLOCK/REFRESH_LOCK matrix
  (409s included), one version snapshot per locked PutFile, 401s for
  bad/expired/cross-file/missing tokens, sharee read-only access
  (can_write false, PutFile/LOCK 403, no-share mint 404), and directory
  rejection (mint 400, callbacks 404).
- `internal/files` seam units: verbatim storage, LockTokenAt, unlock
  mismatch, frozen-clock expiry, incoming-share owner-namespace locking.
- `internal/app` route gate test: flag on → mint 401 + callbacks 401
  (CSRF bypassed); flag off → nothing mounted.
- Config default assertions and six validation counterexamples.

## References

- [WOPI — Web Application Open Platform Interface](https://learn.microsoft.com/en-us/microsoft-365/cloud-storage-partner-program/rest/)
- ADR-0053 (preview generation; Rasterizer precedent), ADR-0102
  (app-token key wraps — the model for the token-bound wrap follow-up),
  ADR-0101/0104 (ErrKeyLocked boundary, ciphertext mounts).
- docs/plans/00-phased-rewrite-plan.md v2 list ("Office
  (Collabora/OnlyOffice integration)").
