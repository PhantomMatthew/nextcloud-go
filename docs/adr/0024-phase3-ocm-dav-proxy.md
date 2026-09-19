# ADR-0024: Phase 3d3 Inbound Federated WebDAV GET

- **Status**: Accepted
- **Date**: 2026-09-20
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3d2 outbound OCM is on local main (`541fd80`). Recipients see a
synthetic DAV mount for inbound shares, but `GET` returned 501. A
cross-instance round-trip needs the receiver to read the file via the
sender's public WebDAV (`/public.php/webdav/` + shared secret).

## Decision

1. **GET/HEAD proxy.** `IncomingMount` carries `RemoteOrigin` and
   `RemoteToken`. `files.DAV.Remote` (`files.RemoteFile`) is implemented by
   `ocm.Client.Get`: `GET {origin}/public.php/webdav/` with Basic `token:`.
2. **File only.** The mount path is the public DAV root (same jail as local
   public-link files). Folder mount GET is 409 (`ErrIsDir`). Nested folder
   paths stay 501.
3. **Writes stay 501.** PUT, DELETE, and MKCOL are not proxied.
4. **SSRF floor.** Only `http`/`https`, 15s timeout, same-host redirects
   (max 5). Private networks are allowed so two local instances can round-trip.
5. **No capability.** `files_sharing.federation` stays false. PROPFIND
   metadata stays synthetic (`Size=0`).

## Alternatives Considered

### Proxy PUT now
- Pros: two-way edit.
- Cons: Range, locks, and permission mapping; out of this GET lock.

### HEAD remote on Stat
- Pros: Size/ETag in PROPFIND.
- Cons: extra RTT on every listing.

## Consequences

### Positive
- A recipient can `GET` `/remote.php/dav/files/{uid}/{name}` and receive
  the remote bytes.

### Negative
- PROPFIND still shows size 0. Folder children are empty. Writes are 501.

### Neutral / follow-ups
- Federation capability, folder tree, write proxy, unshare notifications.

## References

- Open Cloud Mesh API
- ADR-0022
- ADR-0023
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
