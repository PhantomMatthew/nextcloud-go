# ADR-0025: Phase 3d4 Federation Capability

- **Status**: Accepted
- **Date**: 2026-09-20
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3d3 inbound DAV GET is on local main (`348f130`). Outbound
`shareType=6` and inbound GET work, but clients hide federated share
because `files_sharing.federation` was a JSON boolean `false`. Nextcloud
desktop and the files_sharing web UI require
`files_sharing.federation.outgoing === true`.

## Decision

1. **Object, not boolean.** Advertise

   `{outgoing, incoming, expire_date.enabled, expire_date_supported.enabled}`.

2. **Default on.** `DefaultSharingProvider.Federation = true`, so outgoing
   and incoming are true. This matches create + GET already implemented.

3. **No expire UI.** `expire_date` and `expire_date_supported` stay
   `enabled: false`. Local expire jobs are unchanged.

4. **Recapture capabilities goldens** (OCS render fixtures, package
   goldens, HTTP 001/002). Other areas are not recaptured.

## Alternatives Considered

### Boolean `federation: true`
- Pros: one-bit flip.
- Cons: desktop checks `.outgoing`, not truthiness of the node.

### Advertise expire_date_supported
- Pros: closer to PHP when federation app is on.
- Cons: federated expire UI is not implemented.

## Consequences

### Positive
- Desktop can show federated share once it reads capabilities.

### Negative
- Federated sharee search and unshare notify are still missing, so the
  UI path may still fail after the menu appears.

### Neutral / follow-ups
- Folder/write proxy, unshare notifications, federated user search.

## References

- nextcloud/server `apps/files_sharing/lib/Capabilities.php`
- ADR-0023
- ADR-0024
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
