# ADR-0079: Phase 5g addressbook sharing

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0071 (resolves its addressbook-shares skip)

## Context

ADR-0071 imported `oc_dav_shares` rows of `type='calendar'` but left
`type='addressbook'` rows counted-and-skipped: ncgo's `internal/contacts`
had no share table, so there was nothing to map them onto. The same gap was
visible live over CardDAV: an addressbook owner could not share a book with
another local user at all (POST cs:share on an addressbook collection
answered 405, and sharee home listings never showed foreign books).

Nextcloud itself runs one sharing backend for both DAV families:
`apps/dav/lib/DAV/Sharing/Backend.php` is instantiated once per resource
type (`calendar`, `addressbook`) over the same `dav_shares` table, and the
client-visible machinery (cs:share POST, `{uri}_shared_by_{owner}` naming,
`share-access` / `owner-principal` properties) is identical. ncgo already
proved that model for calendars in Phase 3e3 (ADR-0032).

## Decision

Mirror the Phase 3e3 calendar-sharing implementation for addressbooks, one
proven sharing model for both DAV families:

- **Schema**: migration 0019 `addressbook_shares` mirrors
  `0014_calendar_shares` column for column (`addressbook_id` →
  `addressbooks(id) ON DELETE CASCADE`, `target_user_id`, `access` `'read'`
  default, `UNIQUE (addressbook_id, target_user_id)`), in all three
  dialects.
- **Store**: `contacts.Store` gains the same four methods the calendar
  store has (`UpsertAddressbookShare` — UPDATE-then-INSERT upsert with
  `updated_at` bump; `DeleteAddressbookShare`; `ListSharedAddressbooks`;
  `GetSharedAddressbook`), plus `ShareAccessRead` / `ShareAccessReadWrite`
  constants and the `Share` / `SharedAddressbook` (book + OwnerUID +
  Access) types.
- **DAV**: `contacts.DAV` implements `webdav.ShareFS` with the same cs:share
  XML shape and the same rules as calendars: own books only (sharing a
  shared book → 403), target user must exist (400 otherwise), self-share →
  400, empty set+remove → 400, removes tolerate a missing share. Sharees
  see the book under the Nextcloud `{uri}_shared_by_{owner}` naming with
  `Shared`, `share-access`, `owner-principal`, and `PermRead` (or `PermAll`
  for read-write) on the collection entry; object reads, REPORTs
  (addressbook-query/-multiget), and home listings resolve shared books;
  object writes/deletes on a shared book require read-write and route to
  the owner's book. PROPFIND now emits `share-access` / `owner-principal`
  for shared addressbooks (calendars unchanged — the shared-calendar
  golden case pins that exact rendering).
- **No invite state**: shares take effect immediately, matching NC's
  `dav_shares` semantics — every row is an effective share (Backend.php
  hardcodes status accepted), and ncgo's calendar shares already behave
  this way.
- **Sharee cannot delete the collection**: DELETE on a shared book answers
  403, mirroring the calendar rule. This is also NC parity: Nextcloud's
  sharee "unshare" is a separate `dav_shares` row deletion through the
  sharing backend, not a collection DELETE, and ncgo does not ship sharee
  self-unshare for calendars either.
- **Importer**: the ADR-0071 skip branch is replaced by the real mapping
  with the calendar classification rules — user principals only,
  sharee in target, `resourceid` resolving to an addressbook this run (or a
  previous one) actually imported (the resolved-set gate is extended to
  addressbooks, so a share never attaches to a foreign book that merely
  owns the same uri), self-share skipped, `access=3` → `'read'`,
  `access=2` → `'read-write'`, anything else skipped — landing via
  `UpsertAddressbookShare` (created / skipped-same / updated-different).
  The `addressbook shares` report entity now carries real counts; the
  defensive legacy `calendarshares` reading is untouched (that table
  implies `type='calendar'`).

## Alternatives Considered

### Group-principal addressbook shares
- Cons: still deferred for the ADR-0071 reason — expanding a group at
  import is a snapshot: members added later in ncgo would never see the
  book, members removed would keep it. Group DAV sharing belongs in
  ncgo's DAV subsystems first (for calendars and addressbooks alike);
  skipped + warned instead.

### Sharee self-unshare (DELETE on the shared collection)
- Cons: calendar parity — 3e3 deliberately did not ship it, and NC routes
  unshare through the sharing backend rather than collection DELETE. A
  future increment can add it for both DAV families at once.

### A separate sharing model for CardDAV
- Cons: NC uses one backend and one wire shape for both families; a
  divergent ncgo model would buy nothing and double the review surface.
  Mirroring 3e3 keeps the store, DAV, REPORT, and importer code
  line-by-line analogous to the proven calendar path.

## Consequences

- Addressbook owners can share books read or read-write with local users
  over CardDAV; sharees see them under `{uri}_shared_by_{owner}` and
  read-write sharees' writes land in the owner's book.
- `import-nextcloud dav` now writes `addressbook_shares` rows and reports
  real created/updated/skipped counts for them; operators no longer need
  to re-share address books after migration.
- ADR-0071's addressbook-shares follow-up is resolved; group DAV shares
  and sharee self-unshare remain the only deferred sharing items, now for
  both families.

## Verification

- `internal/contacts/share_test.go`: store round trip (create → list with
  OwnerUID, upsert access change, delete, invalid access rejected); DAV
  cs:share set read + read-write + remove; self-share / unknown-user /
  empty-body 400s; sharee home listing carries Shared / ShareAccess /
  OwnerPrincipal; read share blocks object write/delete (403) while
  read-write lets the object land in the OWNER's book (asserted via the
  owner's ListObjects); sharee DELETE on the collection → 403;
  addressbook-query on the shared URI returns the owner's cards.
- `internal/migrations/migrate_test.go`: 19 migrations apply on sqlite,
  `addressbook_shares` present, down-to-18 drops it, re-up restores;
  `integration_test.go` version pin bumped for postgres/mysql.
- `cmd/ncgo-cli/importnc_dav_shares_test.go`: `dav_shares` fixture gains
  addressbook rows — happy-path read import, read-write create, re-run
  access update-down, group / circle / self / unknown-sharee /
  unknown-book / not-imported (owner missing and uri collision) skips,
  NULL access skip; idempotent re-run; dry-run writes nothing.
- `go test ./...` green (shared-calendar golden replay included),
  `go test -race` clean, `golangci-lint run ./...` 0 issues,
  `gofmt -l internal/ pkg/ cmd/` empty, `go mod tidy` no diff.
