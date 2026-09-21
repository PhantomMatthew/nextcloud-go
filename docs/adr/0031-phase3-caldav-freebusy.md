# ADR-0031: Phase 3e2 CalDAV free-busy-query

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3a/3e1 CalDAV could store and query events and todos, but clients
could not ask when a user is busy: `free-busy-query` was unknown to the
webdav REPORT dispatcher, recurring events lost their real duration
(`LastOccur` pushed to 2100 with no expansion), and `TRANSP:TRANSPARENT`
was not parsed.

## Decision

1. **Raw REPORT channel.** New optional `webdav.RawReportFS` interface:
   an FS may answer a REPORT with a raw body and content type instead of
   a multistatus. `free-busy-query` joins the dispatcher's known report
   names. Other report names fall through to the multistatus path.

2. **VFREEBUSY output.** `calendar.DAV.FreeBusy` collects VEVENT objects
   in the time range (all calendars or one collection), skips
   `TRANSP:TRANSPARENT`, expands recurrences, merges overlapping busy
   intervals, and renders a CRLF `VFREEBUSY` document with
   `FREEBUSY;FBTYPE=BUSY` lines. Missing time-range is 400.

3. **RRULE subset.** `parseRRule` supports FREQ=DAILY/WEEKLY/MONTHLY/
   YEARLY with INTERVAL, COUNT, UNTIL. BY* rule parts are parsed over
   and ignored. Expansion caps at 1000 occurrences. An event whose
   RRULE is unexpandable contributes only its first occurrence.

4. **parsedEvent keeps duration.** `parsedEvent` gains `RRule`,
   `Transparent`, and `Dur` (real DTEND-DTSTART or DURATION) so
   recurring events keep their per-occurrence length. Storage is
   unchanged: `first_occur_ms`/`last_occur_ms` keep the Phase 3a wide
   range for recurring events so range queries still match.

5. **Goldens.** 013 PUTs a recurring event (the Phase 3a event is
   deleted by case 009), 014 free-busy-query expands it to three busy
   intervals.

## Alternatives Considered

### Full iCalendar recurrence engine (BYDAY/BYMONTH/EXDATE/RDATE)
- Pros: correct for complex rules.
- Cons: large dependency-free surface; deferred with scheduling.

### VFREEBUSY via multistatus extension
- Pros: no new webdav interface.
- Cons: sabre and clients expect `200 text/calendar`, not 207.

## Consequences

### Positive
- Clients can query busy time; recurring events contribute every
  occurrence within the window.

### Negative
- BY* rules expand as if every interval step occurs (over-busy for
  e.g. `FREQ=WEEKLY;BYDAY=MO`). Documented limitation until scheduling.

### Neutral / follow-ups
- scheduling (iTIP), calendar sharing, EXDATE/RDATE, BY* expansion.

## References

- RFC 4791 §7.10 (free-busy-query), RFC 5545 §3.3.10 (RECUR)
- sabre/dav `freeBusyQuery` handling
