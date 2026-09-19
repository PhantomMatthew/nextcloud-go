# ADR-0021: Phase 3c Notifications and Activity OCS

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3's third slice is the Notifications API and Activity stream. Desktop and
mobile clients poll OCS for the bell and the activity feed. Phase 3b CardDAV is
on local main. Wiring activity/notifications into file DAV writes would recapture
unrelated goldens. `notify_push` is a separate WebSocket surface. A new OCS
capability would rewrite the capabilities golden.

## Decision

1. **SQL rows.** Notifications and activities live in `notifications` /
   `activities` (`0012_notifications_activity`). There is no filecache
   involvement and no producer hooks on PUT/share.
2. **OCS-only.** Authenticated list/get/delete on
   `/ocs/v{1,2}.php/apps/notifications/api/v2/notifications`. Authenticated
   list on `/ocs/v{1,2}.php/apps/activity/api/v2/activity` with `since`,
   `limit`, and `sort`. Unknown activity filters are 404. Empty lists are 200
   with `[]`.
3. **Seed.** Golden replay inserts one admin notification and one
   `file_created` activity in `seedPhase1DAV`. Production has no automatic
   writers in this increment.
4. **ETag.** Notification collection GET honors `If-None-Match` with a SHA1
   fingerprint of id/created_at (non-cryptographic, same class as calendar
   etags). Match returns 304 and an empty body.
5. **No OCS capability.** Clients call the routes directly. Capabilities
   goldens stay unchanged.
6. **Out.** `notify_push`, device registration, notification actions, exists
   POST, activity filters/previews/mail, file-write producers, user-status.

## Alternatives Considered

### Emit on every file write
- Pros: a live stream without seed.
- Cons: couples 3c to files DAV; later increment.

### Advertise notifications/activity capabilities
- Pros: closer to Nextcloud discovery.
- Cons: rewrites capabilities goldens; 3a/3b already skipped this.

## Consequences

### Positive
- Clients can poll, dismiss, and page a seeded feed without touching fileids.

### Negative
- A fresh server has an empty bell until a later producer slice.

### Neutral / follow-ups
- 3d OCM remains later.
- File/share producers and push stay later slices.

## References

- nextcloud/notifications OCS v2
- nextcloud/activity REST API v2
- ADR-0020
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
