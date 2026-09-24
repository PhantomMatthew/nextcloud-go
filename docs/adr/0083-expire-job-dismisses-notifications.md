# ADR-0083: Phase 5j2 background share-expiry dismisses notifications

- **Status**: Accepted
- **Date**: 2026-09-24
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0082 (extends its eager-dismissal coverage)

## Context

ADR-0082's eager dismissal covered the two deletion paths that go through
`sharing.Service` (owner unshare, `expireIfNeeded` on read). A third path
does not: the background `shares.expire` job
(`sharing.NewExpireJob` → `ShareStore.DeleteExpired`) bulk-deletes expired
rows by predicate without ids, so a share reaped there before any read
left its `("share", "ocinternal:<id>")` notification rows behind — a
zombie bell pointing at a share that no longer exists, exactly the state
ADR-0082 set out to eliminate.

## Decision

- `files.ShareStore.DeleteExpired` now returns the **deleted ids**
  (`([]int64, error)`). `SQLShareStore` implements it as
  select-ids-then-delete inside one transaction, so the returned ids name
  exactly the rows removed (no list-then-delete race; a share expiring
  mid-sweep is picked up by the next run). The single implementation and
  single caller made the signature change cheaper than a parallel
  `ExpiredIDs` query method.
- `sharing.NewExpireJob` gains the `ShareNotifier` (nil-gated, the 5j
  interface) and a logger: after a successful sweep it dismisses
  `("share", shareNotifObjectID(id))` for every reaped id. Dismissal is
  best-effort like everywhere else in the 5j design — a bell failure is
  Warn-logged and never fails the sweep. Dismissing a link share's id is
  a harmless no-op (only user/group shares notify).
- Wiring: `app.go` passes the always-constructed `a.notifStore` and the
  app logger.

## Alternatives Considered

### Keep `DeleteExpired` void and add `ExpiredIDs` for the job
- Cons: two round trips with a list/delete gap, and a second
  interface method answering almost the same question. The interface has
  exactly one implementation and one caller; the signature change is the
  honest fix.

### Render-time filtering instead (drop eager dismissal)
- Cons: rejected already in ADR-0082 — eager deletion keeps the polling
  client's list and ETag honest.

## Consequences

- All three share-deletion paths (unshare, read-time expiry, background
  sweep) now dismiss notifications; the zombie-bell state is
  unreachable through any of them.
- `files.ShareStore` implementors (one in-tree, plus the public-link
  test mock) updated; no other store family is touched (locks/trash/
  versions `DeleteExpired` are unrelated interfaces).

## Verification

- `internal/sharing/sqlstore_test.go` `TestSQLShareStoreDeleteExpired`:
  returns exactly the expired share's id, leaves live/never-expiring
  rows, second sweep returns empty.
- `internal/sharing/expire_test.go`
  `TestExpireSharesJobDismissesNotifications`: the job dismisses exactly
  `("share", "ocinternal:<expired-id>")` (live share untouched), a
  failing notifier does not fail the sweep, and the row is gone
  regardless.
- `internal/files/public_test.go` mock updated; full suite green.
