# ADR-0026: Phase 3d5 Federated Sharees

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3d4 advertised `files_sharing.federation.outgoing === true`, so
desktop can show federated share. There was no OCS sharees route, so the
dialog could not turn `bob@https://remote.example.com` into `shareType=6`
`shareWith`. Outbound create already accepts a known cloud ID.

## Decision

1. **GET sharees only.** Authenticated
   `/ocs/v{1,2}.php/apps/files_sharing/api/v1/sharees`. Remainder other
   than empty or `/` is OCS 404 (`/recommended` stays later).

2. **exact.remotes from cloud ID.** `search` is parsed with
   `ocm.SplitCloudID`. A valid uid+remote that is not the request host
   (`sameHTTPHost` vs `RequestBaseURL`, same as outbound create) yields
   one `exact.remotes` row: `shareType=6`, `shareWith` = trimmed search,
   `server` = `NormalizeOrigin(remote)`. Other collections stay empty
   arrays.

3. **No lookup server.** `lookup` and `itemType` are ignored. No HTTP to
   lookup.nextcloud.com. `files_sharing.sharee.query_lookup_default` is
   false; `always_show_unique` is true (PHP default).

4. **Goldens.** Recapture capabilities 001/002. Add sharing 011/012.
   Do not recapture ocm/webdav.

## Alternatives Considered

### Local users/groups on the same endpoint
- Pros: desktop local share dialog uses sharees too.
- Cons: this slice is federated cloud ID only.

### Call lookup.nextcloud.com when lookup=true
- Pros: global user search.
- Cons: external dependency; locked out of increment.

## Consequences

### Positive
- Desktop can resolve a typed cloud ID to shareType 6.

### Negative
- Local user/group typeahead still empty. Unshare notify and folder
  write proxy remain missing.

### Neutral / follow-ups
- sharees/recommended, lookup server, local sharees, folder/write
  proxy, unshare notifications.

## References

- nextcloud/server `apps/files_sharing` ShareesController
- ADR-0023
- ADR-0025
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
