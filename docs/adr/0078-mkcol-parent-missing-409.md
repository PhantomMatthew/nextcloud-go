# ADR-0078: Phase 5f DAV MKCOL parent-missing returns 409, not 404

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0067 (resolves its MKCOL 409 follow-up)

## Context

RFC 4918 §9.3.1: MKCOL on a request URI whose parent collection does not
exist MUST fail with 409 Conflict. The Nextcloud desktop client's mkdir
discovery relies on exactly this signal: it walks up creating parents
when it sees 409.

ncgo's webdav layer has always been ready for this — `writeFSError`
(`internal/webdav/handler.go`) maps `webdav.ErrParentMissing` to 409,
and the handler-level test pins it with a mock FS. The gap was one layer
down: `files.DAV.mkdirOwned` (`internal/files/dav.go`) passed the
backend's error through `mapStorage`, which maps `storage.ErrNotFound`
to `webdav.ErrNotFound` (404). Phase 4u (ADR-0067) made localfs.Mkdir
report a missing parent as `storage.ErrNotFound` (previously a raw 500),
which moved the user-visible behavior from 500 to 404 and left the
404→409 step as a documented follow-up.

## Decision

In `mkdirOwned`, a `storage.ErrNotFound` from `Storage.Mkdir` maps to
`webdav.ErrParentMissing` instead of going through `mapStorage`. In the
mkdir context a missing storage entry can only be the parent (an
existing target yields `storage.ErrExists`, which is already tolerated),
so the translation is exact. `mapStorage` itself is unchanged: every
other operation (GET, PROPFIND, DELETE, ...) legitimately needs 404 for
a missing target.

The incoming-share path (`mkdirMaybeIncoming`, `internal/files/incoming.go`)
delegates to `mkdirOwned` and inherits the fix. Backends align: on S3,
where `Mkdir` is a no-op, a missing filecache parent already failed
`Meta.Insert` with `ErrParentMissing` (409); localfs now agrees.

Out of scope (unchanged, no client evidence): PUT/MOVE/COPY with a
missing parent collection, which RFC 4918 also steers to 409; ncgo keeps
their current mapping until a captured client flow shows otherwise
(wire-compat evidence rule). OCM remote mkdir (`mkdirRemote`) untouched.

## Alternatives Considered

### Fixing in mapStorage globally
- Cons: GET/PROPFIND/DELETE of a missing target MUST stay 404; a global
  remap breaks all of them. Method-scoped translation at the mkdir call
  site is the only correct level.

### Fixing in the webdav handler (MKCOL case rewrites 404→409)
- Cons: the handler cannot distinguish "target missing" from "parent
  missing" — both surface as ErrNotFound from a bare FS. The files layer
  is the one that knows the operation's semantics.

## Consequences

- Real-stack MKCOL `/missing/sub/` now answers 409 on every backend;
  the desktop client's mkdir-up discovery works.
- No change to any other status mapping; the handler-level 409 test
  (mock FS) keeps passing unchanged.

## Verification

- `internal/files/dav_test.go` `TestDAVRoundTrip` gains the assertion:
  `dav.Mkdir(ctx, "alice", "/missing/sub")` returns
  `webdav.ErrParentMissing` (localfs backend).
- `internal/webdav/handler_test.go` `TestHandler_MKCOL_ParentMissing`
  (409 at the HTTP layer) unchanged and green.
- Full `go test ./...` green — no test pinned the old 404.
