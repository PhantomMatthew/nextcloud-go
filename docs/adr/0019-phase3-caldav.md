# ADR-0019: Phase 3a CalDAV events

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3 replaces the bundled PHP `dav` calendar surface. iOS/macOS Calendar.app
and DAVx5 discover calendars via `/.well-known/caldav`, `/remote.php/dav/`,
principals, and `REPORT`. Files DAV has no `REPORT`/`MKCALENDAR`. Storing events
in filecache would shift existing WebDAV fileids.

## Decision

1. **SQL ICS.** Calendars and objects live in `calendars` / `calendar_objects`
   (`0010_calendars`). PUT stores the request body unchanged. GET returns it.
   There is no object-storage or filecache involvement.
2. **Minimal parser.** An in-tree unfold/property reader extracts UID, DTSTART,
   DTEND/DURATION, and RRULE. Exactly one `VEVENT` is required. VTODO/VJOURNAL
   is 415. No new Go module.
3. **RRULE.** Recurring events set `last_occur_ms` to 2100-01-01. Phase 3a does
   not expand recurrences.
4. **TZID.** Without VTIMEZONE lookup, `TZID` date-times are stored as UTC of
   the local clock fields.
5. **Discovery.** `GET /.well-known/caldav` → 301 `/remote.php/dav/`. Principals
   emit `calendar-home-set`. The first PROPFIND on a user's calendar home
   creates `personal` (`#0082c9`).
6. **REPORT.** `calendar-query` (optional `time-range`) and `calendar-multiget`.
   Files DAV without `ReportFS` remains 405. `REPORT` is not added to the global
   files `Allow` list.
7. **No OCS capability.** Clients discover CalDAV via DAV headers
   (`calendar-access`). Capabilities goldens stay unchanged.
8. **Out.** Todos, scheduling, free-busy, calendar sharing, subscriptions,
   CardDAV.

## Alternatives Considered

### ICS in object storage
- Pros: large attachments later.
- Cons: extra backend; Nextcloud stores `calendardata` in SQL.

### Third-party iCalendar library
- Pros: full RFC 5545.
- Cons: new dependency for a VEVENT-only slice.

## Consequences

### Positive
- Calendar.app-style discovery works without touching filecache fileids.
- Files OPTIONS/PROPFIND goldens stay byte-stable.

### Negative
- Recurring events over-match time-range queries until expansion exists.
- TZID without VTIMEZONE is UTC.

### Neutral / follow-ups
- 3b CardDAV can reuse principals and REPORT plumbing.
- Scheduling / free-busy / VTODO stay later slices.

## References

- RFC 4791
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
