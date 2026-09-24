# ADR-0032: Phase 3e3 Calendar Sharing

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3a–3e2 CalDAV covered one user's own calendars only. The webdav
layer already enforces path uid == authenticated user (Phase 1 model),
so cross-user calendar access must follow the PHP design: shared
calendars appear inside the sharee's own calendar home.

## Decision

1. **Storage.** New `calendar_shares` table (migration 0014, all three
   dialects): `calendar_id` + `target_user_id` unique pair, `access` of
   `read` or `read-write`. Store gains Upsert/Delete/ListShared/
   GetShared calendar share methods.

2. **Sharee URI.** A shared calendar is visible to the sharee as
   `{owner_uri}_shared_by_{owner_uid}` in their calendar home, the
   Nextcloud naming. This avoids collisions with the sharee's own
   calendars (e.g. two `personal`). Store calls always use the owner's
   user id and original URI; entry paths use the sharee-visible URI.

3. **Access enforcement.** `DAV.resolveCalendar` resolves own first,
   then shared. Object writes and deletes on a shared calendar require
   `read-write`; calendar-level operations (DELETE collection,
   PROPPATCH, re-share) are owner-only. Reports (calendar-query,
   multiget, free-busy) read through the owner's store handle.

4. **Invite protocol.** POST `cs:share` XML (`set`/`remove` with
   principal href and `read`/`read-write`) on an own calendar. New
   optional `webdav.ShareFS` interface and a POST branch in the webdav
   handler; filesystems without it answer 405. Shares take effect
   immediately — no invite inbox or accept flow. Self-share, unknown
   target, and non-principal hrefs are 400.

5. **Properties.** Shared entries carry `oc:permissions` with the `S`
   flag, `<d:share-access><d:read|read-write/></d:share-access>`, and
   `<nc:owner-principal>`.

6. **Goldens.** 015 POST cs:share admin→bob read-write, 016 PROPFIND of
   bob's home showing the shared calendar, 017 bob PUT into the shared
   calendar.

## Alternatives Considered

### ~~Invite notifications with accept/decline~~
- ~~Pros: matches Nextcloud web UX; sharee controls visibility.~~
- ~~Cons: notification inbox + state machine; deferred as follow-up.~~
  (**Resolved by ADR-0082**: file shares now send NC-faithful bells; DAV
  shares send none because Nextcloud's dav sharing sends none — and an
  invite/accept state machine would contradict the verified no-invite-state
  `oc_dav_shares` semantics of ADR-0071.)

### Group shares
- Pros: parity with file sharing (Phase 2k).
- Cons: needs group membership resolution in the calendar store;
  deferred.

## Consequences

### Positive
- Two users can share a calendar read or read-write over standard
  CalDAV; iOS/macOS and Nextcloud clients discover shares via PROPFIND.

### Negative
- Every sharee write bumps the owner's ctag, so the owner's sync
  clients see sharee changes (intended, but worth noting).

### Neutral / follow-ups
- ~~Invite accept flow~~ (**resolved by ADR-0082**), group shares,
  sharee-initiated unsubscribe, scheduling (iTIP).

## References

- sabre/dav sharing plugin (`cs:share` POST)
- nextcloud/server `apps/dav` CalDavBackend shares
