# ADR-0022: Phase 3d Inbound OCM

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3 includes Open Cloud Mesh federation. Phase 3c is on local main.
A full send/receive round-trip needs outbound HTTP, capability flags, signed
requests, and remote WebDAV proxying. Desktop clients advertise federation
via capabilities; remote Nextcloud servers discover `/.well-known/ocm` and
POST incoming shares without that flag.

## Decision

1. **Inbound only.** `GET /.well-known/ocm` and `GET /ocm-provider` return the
   same discovery JSON. `POST /ocm/shares` persists a row in `ocm_incoming`
   (`0013_ocm_incoming`). OCS `remote_shares` lists, gets, and deletes those
   rows for the authenticated user.
2. **Separate table.** Incoming federated shares have no local owner
   filecache path, so they do not reuse `shares`. `shareType=6` create stays
   400.
3. **DAV metadata mount.** Recipients see a synthetic root entry (`Remote=true`).
   `GET`/`PUT`/`DELETE` on that path return 501. There is no outbound WebDAV
   proxy in this increment.
4. **No capability.** `files_sharing.federation` stays false so capabilities
   goldens are unchanged. Remotes use discovery, not our OCS capabilities.
5. **CSRF.** `/ocm/` is PathBypass so unauthenticated POST from a remote is
   not 412. OCS `remote_shares` is not bypassed.
6. **Auto-accept.** `accepted=1`. Pending mail flow stays later.

## Alternatives Considered

### Reuse `shares` with a dummy owner
- Pros: one table.
- Cons: OwnerUserID and file_path do not describe a remote resource.

### Proxy remote WebDAV now
- Pros: GET works.
- Cons: outbound HTTP, TLS, and token handling; out of this inbound lock.

### Flip federation capability
- Pros: desktop can offer federated share.
- Cons: recaptures capabilities goldens; outbound is not implemented.

## Consequences

### Positive
- A PHP/Go remote can discover us and notify an incoming share. The local
  user can list it over OCS and see a DAV placeholder.

### Negative
- Opening the mounted file is 501 until a later proxy slice.

### Neutral / follow-ups
- Outbound `shareType=6`, notifications, invites, signatures, federated search.

## References

- Open Cloud Mesh API
- ADR-0021
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
