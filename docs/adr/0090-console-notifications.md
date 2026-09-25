# ADR-0090: Phase 5q admin-console surfacing of notifications

- **Status**: Accepted
- **Date**: 2026-09-25
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0082 (resolves its admin-console surfacing deferral)

## Context

ADR-0082 (Phase 5j share notifications) deferred "admin-console surfacing of
notifications (the Phase 5h console could list them later)". The gap is real:
notifications flow through the store — share bells, expiry dismissals — but
only the recipient can see them, per-user, through the OCS bell endpoint. An
admin has no way to answer "what notifications is this instance sending?"
without querying the database by hand. The Phase 5h console (ADR-0080)
already lists status, users, and jobs; notifications are the missing
read-only audit view.

Constraints carried over from ADR-0080:

- Hand-written HTML/JS/CSS, no framework, no build step — the console is
  embedded via `go:embed` and must stay dependency-free.
- Read-only JSON endpoints behind the existing Auth + RequireAdmin chain;
  narrow dependency interfaces on the console `Handler`, nil-gated.
- Pagination shape: `pageParams` clamps limit to [1,200], default 50.

## Decision

1. **`notifications.SQLStore.ListRecent(ctx, limit)`**, mirroring
   `jobs.SQLStore.ListRecent` exactly: nil-store error, `limit <= 0` selects
   the default page size (50), `ORDER BY id DESC LIMIT ?` across **all**
   users, full column set, scan + `rows.Close` error join. The uid shown in
   the console comes from the stored `user_uid` column — no users-table join
   is needed.

2. **Console dependency**: a narrow `console.NotifsStore` interface
   (`ListRecent`) next to `JobsStore`; `Handler.Notifs` is nil-gated like the
   other dependencies (nil → 500 `notifications store unavailable`).

3. **Endpoint**: `GET /console/api/notifications`, dispatched and registered
   exactly like `/console/api/jobs`. Response: `{"notifications": [...]}`
   with items carrying `id`, `user` (uid), `app`, `object_type`,
   `object_id`, `subject`, `message`, `link`, `icon`, `created_at` (RFC3339
   UTC, the jobs endpoint's timestamp convention). The rich
   subject/message templates and their parameters stay out of v1: the shell
   renders plain text via `textContent`, so the pre-rendered `subject` /
   `message` columns cover the audit need and there is nothing that would
   consume the templates.

4. **UI**: a Notifications section under Jobs in `index.html`, rendered by
   `console.js` with the jobs section's exact pattern — fetch on load,
   refresh button, table, empty row, banner error display. No new CSS: the
   shared table/empty/button rules cover it.

5. **Scope is read-only.** No dismiss/delete affordance in the console; the
   per-user OCS endpoint remains the only mutation path.

## Alternatives Considered

### Per-user view only (admin picks a uid, sees that user's list)
- Pros: reuses the existing `List(ctx, userID)`.
- Cons: the admin use-case is instance-wide — "is the instance emitting the
  right bells?", not "impersonate one user's bell". A uid picker also adds
  UI the no-framework shell does not need. Rejected; per-user visibility
  already exists through the OCS endpoint.

### Joining the users table for the uid
- Cons: `user_uid` is already denormalized onto the notification row
  (written at insert, kept current with the row); a join buys nothing and
  couples the read to the users schema. Rejected.

### Exposing the rich-subject fields
- Pros: stock-client-faithful rendering.
- Cons: the console has no rich-parameter renderer (that machinery lives in
  NC clients, not in our no-build shell); shipping raw template JSON would
  render worse than the plain subject. Deferred — the pre-rendered subject
  and message cover the audit need.

## Consequences

- Admins see the newest notifications instance-wide, newest first, from the
  same gated console surface as jobs — closing the last visibility gap in
  the notifications subsystem.
- The read is one indexed query with a bounded limit; no write path, no
  join, no new failure modes beyond the nil-store 500 shared with the other
  console views.
- Rich templates stay server-internal until a console-side renderer exists;
  the OCS bell payload is unaffected.

## Verification

- `internal/notifications/sqlstore_test.go`: `ListRecent` on an empty store
  returns empty; rows seeded across two users come back newest-first with
  the uid carried from the stored column and the limit honored; a nil store
  errors.
- `internal/console/console_test.go`: an authenticated admin GET returns
  the seeded notifications with the pinned fields and RFC3339 timestamps,
  `?limit=` flows through, and a nil `Notifs` dependency answers the same
  500 JSON error as the other views (authz itself is covered generically by
  the middleware tests).
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` clean (no new
  dependencies).
