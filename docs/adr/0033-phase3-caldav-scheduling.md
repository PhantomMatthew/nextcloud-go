# ADR-0033: Phase 3e4 CalDAV Scheduling (iTIP)

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3a–3e3 CalDAV covered personal and shared calendars, but inviting
another user to an event did nothing: ORGANIZER/ATTENDEE lines were
stored verbatim and no invitation ever reached the attendee.

## Decision

1. **Local iTIP only.** Scheduling works between users of the same
   server, matched by `mailto:{user.email}`. `users.Store` gains
   `GetByEmail`. External or unknown attendee emails are silently
   skipped — no iMIP mail sending.

2. **REQUEST/UPDATE on organizer write.** When a write to an own
   calendar carries an ORGANIZER equal to the writer's email and at
   least one ATTENDEE, the raw payload is delivered to each local
   attendee's default (`personal`) calendar at `{UID}.ics` (existing
   copy URI is reused on update).

3. **REPLY on attendee write.** When the writer is an ATTENDEE and the
   ORGANIZER is a local user, the writer's own copy stores normally and
   the organizer's copy gets its ATTENDEE line rewritten with the new
   PARTSTAT (`updateAttendeePartstat`, line-based, \r style preserved,
   folded lines not matched).

4. **CANCEL on organizer delete.** Deleting an own event the writer
   organized removes the object with the same UID from every local
   attendee's default calendar.

5. **No-op escapes.** A writer without an email, events without
   ORGANIZER/ATTENDEE, and shared-calendar writes skip scheduling
   entirely.

6. **Discovery.** Principal entries emit
   `cal:calendar-user-address-set` with `mailto:{email}` when the user
   has an email.

7. **Goldens.** Seed gives admin/bob example.com emails (recapture
   003). Cases 018–023 cover invite, delivery, accept, organizer view,
   cancel, and 404-after-cancel.

## Alternatives Considered

### schedule-inbox / schedule-outbox collections (RFC 6638)
- Pros: full scheduling discovery; async processing.
- Cons: two more collections plus a queue; direct delivery covers the
  local two-user case.

### iMIP email for external attendees
- Pros: real-world invites outside the server.
- Cons: needs an SMTP stack; deferred.

## Consequences

### Positive
- Two local users can run the full invite → accept/decline → cancel
  round-trip over plain CalDAV writes.

### Negative
- No SEQUENCE negotiation: a stale client write silently wins, same as
  a last-write-wins file PUT.

### Neutral / follow-ups
- iMIP, inbox/outbox, SEQUENCE, group attendees, attendee-side delete
  (decline by removal).

## References

- RFC 5546 (iTIP), RFC 6638 (CalDAV scheduling)
- nextcloud/server `apps/dav` schedule plugin
