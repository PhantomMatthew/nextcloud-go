# ADR-0082: Phase 5j file-share notifications (bells)

- **Status**: Accepted
- **Date**: 2026-09-24
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0032 (resolves its invite-notifications follow-up)

## Context

ADR-0032 deferred "invite notifications with accept/decline" as a follow-up.
Phase 3c shipped the notifications subsystem (store + OCS endpoint) but no
producer: creating a file share left the sharee's bell empty. Nextcloud's
`apps/files_sharing` sends an OCS notification on every incoming user/group
share (`Notification/Listener.php` + `Notifier.php`); clients poll the
notifications endpoint and render the bell from the rich payload. ncgo must
emit the same shape so stock clients light up.

## Decision

Payload shape is researched verbatim from Nextcloud's
`apps/files_sharing/lib/Notification/{Listener,Notifier}.php`:

- **Object identity**: app `files_sharing`, object type `share`, object id
  `ocinternal:<shareID>` — NC's `getFullId()` is `providerId:id` with the
  internal provider id `ocinternal`.
- **User share**: one notification to the sharee. Rendered subject (ncgo has
  no l10n, so the English rendering is stored): `You received <path> as a
  share by <sharer displayname>`; rich template `You received {share} as a
  share by {user}`; rich parameters `share:{type:"highlight",
  id:<objectID>, name:<path>}` and `user:{type:"user", id:<sharer uid>,
  name:<sharer displayname>}`, marshalled as compact JSON.
- **Group share**: one notification per group member **except the actor**
  (in ncgo's `Create` the sharer is always the owner), resolved via
  `Users.GroupMembers(gid, 0)`. Subject `You received <path> to group <gid>
  as a share by <sharer displayname>`; template `You received {share} to
  group {group} as a share by {user}`; parameters add
  `group:{type:"user-group", id:<gid>, name:<group displayname>}`.
- **No actions**: NC's accept/reject actions exist only for pending shares;
  ncgo auto-accepts (`Accepted: 1`), and ncgo's Notification carries no
  actions field, so `actions` stays empty. `should_notify` is true; message,
  messageRich, link, and icon stay empty (ncgo has no icon route for this
  app).
- **Eager dismissal**: unshare (`Service.Delete`), read-time expiry
  (`expireIfNeeded`), and the background `shares.expire` sweep (ADR-0083)
  delete the rows of **all** users for
  `("share", "ocinternal:<id>")` via the new
  `notifications.SQLStore.DeleteByObject`. Nextcloud instead filters
  dead-share notifications lazily at render time; ncgo deletes eagerly,
  which is strictly better for polling clients — a dismissed share stops
  bumping the list ETag instead of lingering as a zombie the client must be
  taught to ignore.
- **A bell never fails a share**: notification insert failures are
  Warn-logged and share creation continues; dismiss failures are Warn-logged
  and the delete's outcome stands. The dependency is an interface
  (`sharing.ShareNotifier`, satisfied by `*notifications.SQLStore`) exactly
  so the failure-continues path is testable with a failing stub.

**DAV verdict (closes the ADR-0032 follow-up)**: calendar and addressbook
shares send **no** notifications. Verified: `apps/dav` sharing paths have no
notification-manager usage at all (only Activity providers), so NC-faithful
means silent here. An invite/accept state machine on top would contradict
the verified no-invite-state `oc_dav_shares` semantics (ADR-0071) that both
DAV sharing backends implement.

## Alternatives Considered

### Lazy filtering like Nextcloud (keep rows, hide at render)
- Pros: zero write on unshare; row survives as an audit trail.
- Cons: every notifications list must join back to the shares table to
  filter zombies; polling clients keep seeing ETag churn for dead shares;
  two subsystems stay coupled forever. Eager delete is one indexed DELETE
  on a path that already writes, and the share row itself is gone — the
  notification has nothing left to point at.

### Reuse the Activity subsystem as the notification producer
- Cons: NC itself keeps the two streams separate (files_sharing talks to
  the notification manager directly, not via Activity); folding them would
  invent semantics NC clients do not expect (bells are dismissible per
  object, activity is a permanent feed). Activity entries for shares remain
  the activity subsystem's own roadmap.

## Consequences

- User and group file shares light up the sharee's bell in stock clients
  with NC-exact app/object/subject/rich payloads; unshare and expiry make
  the bell disappear for every recipient at once.
- Link and remote (OCM) shares stay silent, as in NC.
- Notification writes sit off the share transaction's critical path: a
  notifications outage degrades to missing bells, never to failed shares.
- Group-join backfill (NC's `userAddedToGroup` listener) is deferred: ncgo
  group membership changes are CLI/import-driven today; revisit if a
  group-membership API ships. Remote-share notifications (OCM subjects) and
  admin-console surfacing of notifications (the Phase 5h console could list
  them later) are likewise deferred.

## Verification

- `internal/notifications`: `DeleteByObject` deletes the rows of all users
  for one type+id, leaves other ids/types untouched, and treats a missing
  object as a no-op.
- `internal/sharing`: user-share create inserts exactly one NC-exact
  notification for the sharee (app/object id/template/parsed rich params,
  `ocinternal:` id, `ShouldNotify`, empty message/link/icon); group-share
  create fans out to every member minus the actor; link and remote creates
  insert none; a failing notifier fails neither `Create` (share still
  inserted) nor `Delete`; `Delete` and the backdated-expiry path both
  dismiss `("share", "ocinternal:<id>")`.
- `internal/app`: end to end through the OCS endpoints — create share → the
  sharee's notifications GET lists the bell → unshare → the list is empty.
- Full `go test ./...` green; `golangci-lint run ./...` 0 issues; no new
  dependencies.
