# ADR-0028: Phase 3d7 OCM SHARE_UNSHARED Notifications

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3d6 inbound folder and write proxy is on local main (`e300e0e`).
Creating an outbound federated share notifies the remote via
`POST /ocm/shares`, but deleting that share only removed the local row.
The recipient's `ocm_incoming` mount stayed until manual
`remote_shares` delete.

## Decision

1. **Outbound unshare is best-effort.** `sharing.Service.Delete` for
   `shareType=6` Discover's the remote then `POST {endPoint}/notifications`
   with `notificationType=SHARE_UNSHARED`, `resourceType=file` (PHP sends
   `file` even for folders), `providerId` equal to the local share id
   string, and `notification.sharedSecret` equal to the share token.
   Discover or notify failure does not fail the OCS delete; the local
   row is always removed.

2. **Inbound handler.** Public `POST /ocm/notifications` on the existing
   `/ocm/` prefix accepts only `SHARE_UNSHARED`. Lookup is
   `remote_id`+`token`. Success is HTTP 201 with body `[]`. Missing
   fields, unknown types, and unknown shares are 400. Unsupported
   `resourceType` is 501. No HTTP Message Signatures.

3. **Goldens.** 014 deletes the outbound share from 006. 015 unshares
   inbound providerId 42. 016 GET of that mount is 404.

## Alternatives Considered

### Fail OCS delete when notify fails
- Pros: remote stays in sync or owner retries.
- Cons: a down remote traps the local share; PHP deletes locally anyway.

### Recipient remote_shares DELETE notifies origin
- Pros: SHARE_DECLINED round-trip.
- Cons: locked out of this increment.

## Consequences

### Positive
- Owner unshare removes the recipient mount when both instances are
  nextcloud-go (or PHP that implements SHARE_UNSHARED).

### Negative
- Recipient-initiated delete still does not tell the owner. Lookup
  server and CalDAV leftovers remain.

### Neutral / follow-ups
- lookup.nextcloud.com, VTODO/scheduling, notify producers/push,
  SHARE_ACCEPTED/DECLINED.

## References

- ADR-0023
- Nextcloud `federatedfilesharing` Notifications.php SHARE_UNSHARED
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
