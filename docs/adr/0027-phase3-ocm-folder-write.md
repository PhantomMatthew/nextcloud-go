# ADR-0027: Phase 3d6 Inbound Folder and Write DAV Proxy

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3d5 federated sharees is on local main (`c0ff365`). Recipients can
GET a federated file, but folder PROPFIND was empty, nested paths were
501, and PUT was 501. A folder round-trip needs Depth 1 listing and
permission-gated writes through the sender's public WebDAV.

## Decision

1. **RemoteFile grows.** `Propfind` (depth 0 or 1), `Put`, `Delete`,
   `Mkcol` join `Get`. `ocm.Client` implements them against
   `{origin}/public.php/webdav/{rel}` with the same SSRF floor as 3d3.

2. **Parse 207.** `webdav.ParseMultistatus` reads href / collection /
   getcontentlength / getetag / getlastmodified / getcontenttype.

3. **Folder list and nested GET.** Folder mount GET stays 409. Nested
   GET uses `remoteRel`. List calls Propfind depth 1. Nested Stat uses
   Propfind depth 0 (not the mount ItemType).

4. **Writes are permission-gated.** Optional
   `protocol.options.permissions` (default `PermRead`). Read-only PUT is
   403. Folder mount root MKCOL/DELETE stay 403. MOVE/COPY stay 403.

5. **Goldens.** 009 PUT file is 403. 010–013 cover folder incoming,
   PROPFIND, nested GET, and writable PUT.

## Alternatives Considered

### Depth infinity recursion
- Pros: desktop folder trees in one round-trip.
- Cons: locked out; FS.List is one level.

### Proxy MOVE/COPY
- Pros: rename on the remote.
- Cons: Destination mapping; incoming MOVE is already 403.

## Consequences

### Positive
- Recipients can browse a federated folder and edit children when
  permissions allow.

### Negative
- Unshare still does not notify the remote. Lookup server and CalDAV
  leftovers remain.

### Neutral / follow-ups
- `POST /ocm/notifications`, lookup server, VTODO/scheduling, notify
  producers/push.

## References

- ADR-0024
- ADR-0025
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
