# ADR-0051: Phase 4e4 import-nextcloud dav (calendars and address books)

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phases 4e1–4e3 shipped `import-nextcloud` scaffolding plus the `users`,
`files`, and `shares` subcommands (ADR-0048/0049/0050). 4e4 completes the
series with the `dav` subcommand: migrating calendars (`oc_calendars` +
`oc_calendarobjects`) and address books (`oc_addressbooks` + `oc_cards`) from
a PHP Nextcloud database into ncgo's DAV schema (migrations 0010/0011).

Unlike files and shares, DAV objects are *parsed* by ncgo: the production
write path (`calendar.SQLStore.PutObject`, `contacts.SQLStore.PutObject`)
derives the iCalendar/vCard UID, component type, first/last occurrence, size,
and etag from the object bytes, and bumps the collection ctag. The importer
must decide how much of Nextcloud's pre-computed metadata to trust versus
recompute, and how to handle sync tokens.

## Decision

1. **Objects go through the production `PutObject` path, not direct inserts.**
   Every calendar object and card is written with the same store method a
   client PUT uses (`internal/calendar/sqlstore.go`,
   `internal/contacts/sqlstore.go`). This keeps the indexed columns
   (`uid`, `component`, `first_occur_ms`, `last_occur_ms`, `fn`, `size`,
   `etag`) consistent with what ncgo's own queries (range reports, UID
   lookups, sync listings) expect, and gives malformed-data detection for
   free. Nextcloud's pre-computed `componenttype`/`firstoccurence`/
   `lastoccurence`/`uid`/`size` columns are therefore *not* read — recomputing
   them is strictly safer than trusting a cache.

2. **etag, synctoken/ctag, and lastmodified are regenerated — clients resync
   once.** ncgo's etag is the SHA-1 of the object bytes (deterministic, so
   identical content gets an identical etag); Nextcloud's etag strings are
   dropped. ncgo has no synctoken column: `ctag` starts at 1 and is bumped by
   `PutObject` per object, exactly as in production. `PutObject` sets
   `updated_at` to the write time and offers no seam to preserve
   `lastmodified`, so the source `lastmodified` is dropped as well.
   Consequence: every DAV client must perform one full resync after
   migration. This is the accepted trade-off of the 4e series (sessions and
   app passwords already force re-login); preserving Nextcloud's sync tokens
   would require bypassing the production invariants for zero lasting
   benefit.

3. **Field mapping — calendars.**

   | oc_calendars | ncgo `calendars` | Notes |
   |---|---|---|
   | `principaluri` | `user_id` | must be `principals/users/<uid>`, resolved via the users store; other principal forms or unknown owners → skip+warn (objects counted skipped) |
   | `uri` | `uri` | verbatim |
   | `displayname` | `displayname` | verbatim (NULL/'' → store default: the uri) |
   | `description` | `description` | verbatim (NULL → '') |
   | `calendarcolor` | `calendar_color` | verbatim (NULL/'' → store default `#0082c9`) |
   | `calendarorder` | `calendar_order` | verbatim |
   | `timezone` | `timezone` | verbatim (NULL → '') |
   | `components` | — | **dropped** (ncgo has no per-calendar components field; component types are stored per object) — one summary warning |
   | `transparent` | — | **dropped** (no ncgo counterpart) — one summary warning when any calendar had it set |
   | `synctoken` | `ctag` | **not preserved** — fresh ctag, bumped per imported object (decision 2) |
   | — | `enabled` | 1 (ncgo default) |

   Nextcloud's birthday calendar (`uri = contact_birthdays`) is a regular
   `oc_calendars` row and imports normally.

4. **Field mapping — calendar objects.**

   | oc_calendarobjects | ncgo `calendar_objects` | Notes |
   |---|---|---|
   | `calendarid` | `calendar_id` | resolved through the imported target calendar |
   | `uri` | `uri` | verbatim |
   | `calendardata` | `calendar_data` | **verbatim bytes**; empty/whitespace → skip+warn |
   | `lastmodified` | `updated_at` | **regenerated** (import time — `PutObject` has no preservation seam) |
   | `etag` | `etag` | **regenerated** (SHA-1 of data) |
   | `uid`, `componenttype`, `firstoccurence`, `lastoccurence`, `size` | computed columns | **recomputed by `PutObject`**, source values not read |
   | `classification` | — | **dropped** (no ncgo counterpart) |

   Objects `PutObject` rejects (unparseable ICS, VJOURNAL or mixed
   VEVENT+VTODO — both unsupported by ncgo, or a UID already present under a
   different uri in the same calendar) are skipped with a warning, never
   failed the run.

5. **Address books and cards: analogous.**

   | oc_addressbooks | ncgo `addressbooks` | Notes |
   |---|---|---|
   | `principaluri`, `uri`, `displayname`, `description` | same as calendars | identical handling |
   | `synctoken` | `ctag` | not preserved (decision 2) |

   | oc_cards | ncgo `addressbook_objects` | Notes |
   |---|---|---|
   | `carddata` | `card_data` | verbatim bytes; empty → skip+warn |
   | `uri` | `uri` | verbatim |
   | `uid` | `uid`, `fn` | recomputed by `PutObject` |
   | `lastmodified`, `etag`, `size` | — | regenerated as for calendar objects |

6. **Calendar shares are deferred with a counted warning.** ncgo has calendar
   sharing (`calendar_shares`, migration 0014, Phase 3e3), but Nextcloud's
   `oc_calendarshares` (or `oc_dav_shares` on newer versions) carries
   invite/accept state and group principals that have no clean v1 mapping.
   The importer counts whichever table exists and reports the rows as skipped
   with one warning ("re-share calendars after migration"); it never writes
   `calendar_shares`. Follow-up: map accepted user shares to
   `calendar_shares` rows.

7. **Idempotency keys.** A calendar/addressbook is skipped when owner+uri
   already exists in the target. If the existing collection's mapped
   properties match (displayname/description/color/order/timezone after store
   defaults), the run is a resume: missing objects are imported individually
   (existing ones skipped by uri), so an interrupted run completes on
   re-run. If the properties differ, the existing collection is foreign — a
   genuine collision — and the source collection is skipped **wholesale**
   (objects counted skipped, warning printed) rather than merging objects
   into someone else's calendar.

8. **Reporting.** Entities: `calendars`, `calendar objects`, `addressbooks`,
   `cards`, `calendar shares` (skipped-only), each as `N created, K skipped
   (existing), J failed` via the 4e1 `importReport`; warnings capped at 20
   printed plus a count. Skipped collections contribute their object rows to
   the skipped counts so every source row is accounted for. `--dry-run`
   scans and counts everything (including per-object empty-data skips) and
   writes nothing.

## Alternatives Considered

### Direct SQL inserts preserving lastmodified and etag
- Pros: clients could in theory keep their sync state; timestamps stay
  meaningful for debugging.
- Cons: bypasses `PutObject`'s parsing, so `uid`/`component`/occurrence
  columns would have to be trusted from Nextcloud's cache (stale caches are
  common) or re-parsed anyway; requires a new non-production write path in
  the stores for a one-shot tool; and the sync-state benefit is illusory
  because ncgo's ctag semantics differ from Nextcloud's synctoken regardless
  — clients must resync either way. Rejected.

### Importing calendar shares as read-only grants
- Pros: shared calendars appear for sharees immediately after migration.
- Cons: invite state (pending/accepted), group shares, and access-bit
  differences (Nextcloud's bitmask vs ncgo's read/read-write) make a partial
  mapping worse than none — sharees would see calendars they never accepted.
  Deferred; counted warning instead.

### Preserving Nextcloud etags verbatim
- Pros: byte-level continuity of the ETag header.
- Cons: ncgo regenerates the etag on the first subsequent PUT anyway, and a
  preserved foreign etag that *disagrees* with ncgo's SHA-1 convention would
  confuse If-Match handling in edge cases. Regeneration is uniform.

## Consequences

- `ncgo-cli import-nextcloud dav --source-driver … --source-dsn …` imports
  calendars, their objects, address books, and cards after `users` has run,
  printing the shared summary format. **This completes the 4e
  import-nextcloud series** (users → files → shares → dav).
- Imported collections behave exactly like client-created ones: identical
  bytes, ncgo-computed index fields, fresh ctags. DAV clients must resync
  once and users must re-share calendars.
- Empty/unparseable object data, unknown owners, non-user principals, and
  uri collisions are explicit skips with warnings — never silent data loss,
  never merged into foreign collections.
- Re-running the import is a no-op (all skipped); resuming after a partial
  failure imports only the missing objects; `--dry-run` writes nothing.

## Verification

- `cmd/ncgo-cli/importnc_dav_test.go`: source fixture with two calendars for
  alice (one fully populated with color/order/timezone/components, one with
  NULLs + transparent flag), a calendar for an unknown owner, a group-principal
  calendar, VEVENT + VTODO + empty-data + orphaned-calendar objects, two
  address books (one unknown owner), and cards including an empty-data row,
  plus `oc_calendarshares` rows — imported into a migrated temp sqlite target
  with alice. Per-entity counts and every warning asserted; calendars and
  objects verified through the `calendar.SQLStore`/`contacts.SQLStore` read
  methods (ListCalendars/GetCalendarByURI/ListObjects/GetObject/
  ObjectsInRange/ListBooks): verbatim ICS/vCard bytes, mapped metadata,
  recomputed UID/component/first-occurrence, SHA-1 hex etag replacing the
  source etag, lastmodified regenerated, ctag = 1 + objects imported.
  `--dry-run` counts correctly and writes zero rows; second run all-skipped
  with flat object counts; uri collision with a differently-propertied
  pre-existing calendar skips wholesale and leaves it untouched;
  `ncPrincipalUID` unit-tested.
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` no diff.
