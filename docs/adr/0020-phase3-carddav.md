# ADR-0020: Phase 3b CardDAV address books

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3's second slice is RFC 6352 CardDAV. iOS Contacts.app discovers books via
`/.well-known/carddav`, `/remote.php/dav/`, principals' `addressbook-home-set`,
and `REPORT` addressbook-query/multiget. Phase 3a already mounts principals and
REPORT plumbing, but REPORT names and xmlns were CalDAV-only. Storing vCards in
filecache would shift existing WebDAV fileids. A new OCS capability would rewrite
the capabilities golden.

## Decision

1. **SQL vCard.** Address books and objects live in `addressbooks` /
   `addressbook_objects` (`0011_addressbooks`). PUT stores the request body
   unchanged. GET returns it. There is no object-storage or filecache involvement.
2. **Minimal parser.** An in-tree unfold/property reader requires exactly one
   `VCARD` and a `UID`. `FN` is stored as a column. `text-match` is ignored in
   3b, matching 3a's ignored `prop-filter`. No new Go module.
3. **Path prefix.** Collections are mounted at
   `/remote.php/dav/addressbooks/users/{uid}/` so existing `parsePath` still
   treats the first segment as uid. The default book is `contacts` /
   displayname `Contacts`.
4. **Discovery.** `GET /.well-known/carddav` → 301 `/remote.php/dav/`.
   Principals and the DAV root emit `addressbook-home-set`. The first PROPFIND
   on a user's addressbook home creates `contacts`. Root `DAV` advertises
   `addressbook` (caldav goldens 002/003 recaptured).
5. **REPORT.** `addressbook-query` and `addressbook-multiget`. Files DAV without
   `ReportFS` remains 405. `REPORT` is not added to the global files `Allow`
   list. CardDAV xmlns `urn:ietf:params:xml:ns:carddav` is emitted only when
   `PropfindContext.CardDAV` is set.
6. **No OCS capability.** Clients discover CardDAV via DAV headers
   (`addressbook`). Capabilities goldens stay unchanged.
7. **Out.** KIND:group collections, shared address books, GAL, Circles,
   PHOTO size policy beyond the 4MiB PUT cap, CalDAV VTODO/scheduling.

## Alternatives Considered

### vCard in object storage
- Pros: large PHOTO later.
- Cons: extra backend; Nextcloud stores card data in SQL.

### Third-party vCard library
- Pros: full RFC 6350.
- Cons: new dependency for a UID/FN-only slice.

### Drop the extra `users` path segment
- Pros: closer to CalDAV `/calendars/{uid}/`.
- Cons: would require a CardDAV-specific path parser.

## Consequences

### Positive
- Contacts.app-style discovery works without touching filecache fileids.
- Files OPTIONS/PROPFIND goldens stay byte-stable.

### Negative
- `text-match` filters are ignored until a later slice.
- Same-book UID uniqueness is enforced in Store, not by a unique SQL index.

### Neutral / follow-ups
- 3c notifications and 3d OCM remain later.
- Shared address books and group collections stay later slices.

## References

- RFC 6352
- ADR-0019
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
