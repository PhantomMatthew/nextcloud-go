# ADR-0030: Phase 3e1 CalDAV VTODO Support

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3a (ADR-0019) shipped CalDAV events only: `parseICS` rejected
VTODO/VJOURNAL with `ErrUnsupported`, `calendar-query` ignored the
comp-filter component name, and PROPFIND advertised only VEVENT in
`supported-calendar-component-set`. Task apps (iOS Reminders, Tasks.org)
sync via VTODO and could not use the server.

## Decision

1. **One component per object.** `parseICS` accepts exactly one VEVENT
   or one VTODO. VJOURNAL and mixed VEVENT+VTODO payloads stay
   `ErrUnsupported`; a payload with neither component is `ErrInvalid`.

2. **VTODO time anchors.** UID is required; DTSTART is optional (RFC
   5545). FirstOccur/LastOccur anchor order: DTSTART (LastOccur = DUE,
   else DTSTART+DURATION, else DTSTART), then DUE, then COMPLETED, then
   CREATED. A todo with no dates stores zero FirstOccur/LastOccur and
   matches only range-unbounded queries, mirroring PHP
   `oc_calendarobjects` semantics.

3. **comp-filter honored.** `calendar-query` parses the innermost
   `comp-filter name` (VEVENT/VTODO) plus `time-range`; results filter
   by `calendar_objects.component` when a name is present. No
   comp-filter keeps the Phase 3a behavior (all components).

4. **Advertisement.** PROPFIND `supported-calendar-component-set` now
   lists VEVENT and VTODO. No schema migration: the `component` column
   exists since Phase 3a (`0010_calendars`).

5. **Goldens.** Recapture caldav 004 (component-set). Add caldav
   011-put-todo and 012-report-query-vtodo.

## Alternatives Considered

### RRULE expansion and free-busy in the same increment
- Pros: complete RFC 4791 read surface at once.
- Cons: expansion is a large, separable concern; locked out of 3e1.

### Per-calendar supported-components from MKCALENDAR
- Pros: task-only calendars like PHP.
- Cons: the calendars table has no components column; a migration for
  a cosmetic prop is not worth this increment.

## Consequences

### Positive
- Task apps can sync todos; clients can discover VTODO support from
  PROPFIND.

### Negative
- A no-date todo is invisible to time-range queries until it gains a
  date, same as PHP.

### Neutral / follow-ups
- free-busy-query (needs RRULE expansion), scheduling (iTIP), calendar
  sharing.

## References

- RFC 5545 §3.6.2 (VTODO), RFC 4791 §9.9 (time-range filtering)
- nextcloud/server `apps/dav` CalDavBackend calendar object handling
