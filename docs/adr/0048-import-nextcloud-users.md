# ADR-0048: Phase 4e1 import-nextcloud Scaffolding and Users/Groups Import

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

The phased rewrite plan promises `ncgo-cli import-nextcloud` so existing
Nextcloud admins can migrate from a PHP instance to nextcloud-go. The
migration surface is large (users/groups, files, shares, calendars/contacts),
so it is delivered as a 4e series of increments: **4e1 = scaffolding +
users/groups** (this ADR), 4e2 = files, 4e3 = shares, 4e4 = dav
(calendars/contacts). The scaffolding must let later increments slot in as
sibling subcommands with minimal churn.

## Decision

1. **Direct source-database reads.** The importer opens a second
   `database.DB` connection to the PHP Nextcloud database
   (`--source-driver` mysql|postgres|sqlite, `--source-dsn`,
   `--table-prefix` default `oc_`) and reads `oc_*` tables directly. The
   target is the configured ncgo database via the existing
   `loadConfig`/`openDB` helpers. No dump-and-restore, no PHP tooling, no
   Nextcloud-version-specific API: the `oc_users` / `oc_groups` /
   `oc_group_user` schema is stable across supported Nextcloud versions.
   Source queries use `?` placeholders; `database.DB` rebinds them for
   Postgres, exactly as the ncgo stores do. The table prefix is validated
   (`[A-Za-z0-9_]+`) before being interpolated into SQL, since SQL
   identifiers cannot be parameterized.

2. **Phased subcommand plan.** `import-nextcloud` is a parent command
   carrying the source flags as persistent flags plus a shared
   source-open helper and an `importReport` type (per-entity
   created/skipped/failed counters, capped warning list, printed summary).
   4e1 ships the `users` subcommand; 4e2–4e4 add `files`, `shares`, and
   `dav` siblings in the same file/package.

3. **Password policy.** Source `oc_users.password` values starting with
   `$argon2id$` are imported **verbatim**: PHP's `password_hash` emits
   standard argon2id PHC strings (`$argon2id$v=19$m=...,t=...,p=...$salt$hash`)
   and ncgo's `internal/auth` Argon2id verifier parses exactly that format,
   so those users keep their passwords (verified by an end-to-end test that
   authenticates an imported user through `users.PasswordVerifier`). Every
   other format (bcrypt `$2y$`, argon2i, legacy hashes, empty) is replaced
   with the sentinel `"!"`, which is not a parseable PHC string and never
   verifies; a per-user warning is printed and the user must reset their
   password. ncgo has no bcrypt verifier, and adding one just for migration
   would expand the auth attack surface for a one-time operation.

4. **UID mapping.** Source uids are imported verbatim when they satisfy
   ncgo's uid rules (non-empty; no whitespace, control characters, `/`, or
   `\` — characters that break the DAV URL and file-path contexts uids
   appear in). Otherwise `uid_lower` is tried; if neither is valid the user
   is skipped with a warning. Successful remaps are also warned, since the
   login name changes. The source-uid → target-uid mapping is kept so
   `oc_group_user` rows resolve to the imported uid.

5. **Sessions and app passwords are NOT imported.** Nextcloud authtokens
   and app-password tokens are cryptographically bound to the source
   instance secret; importing them would produce dead rows at best. Users
   must log in again and reissue app passwords after migrating. This is a
   deliberate deviation from the plan's full `import-nextcloud` list
   ("Users, groups, app passwords, sessions"), recorded here for the whole
   4e series and stated in the command's long help. (Refined by ADR-0071:
   the binding turned out to be a plain `sha512(token+secret)` hash that
   ncgo mirrors exactly, so app passwords — not sessions — are importable
   when the operator carries the secret over; the no-import-code decision
   stands, with the carry-over recipe and a `tokens` follow-up recorded
   there.)

6. **Idempotent, resumable, per-entity commits.** Every write is checked
   against the target first: existing users/groups/memberships are skipped
   unchanged (never overwritten), and `AddGroupMember` already ignores
   unique violations. Writes are committed per entity (plain autocommit, no
   wrapping transaction), so an interrupted run is simply repeated. Per-row
   failures increment a failed counter and warn rather than aborting the
   import.

7. **`--dry-run`.** Performs the full source scan, counting, and warning
   output with zero target writes (planned users/groups are tracked in
   memory so membership accounting is accurate) and prints
   `dry-run: no changes written`.

## Alternatives Considered

### SQL dump + transform pipeline
- Pros: works without live source DB access; offline.
- Cons: another artifact format to version and test; dump formats differ
  per database; the live-read approach is simpler and the source DB is
  reachable during a planned migration window. A dump can still be loaded
  into a scratch database and imported from there.

### Verifying bcrypt hashes during import (import bcrypt verbatim)
- Pros: more users keep their passwords.
- Cons: requires shipping a bcrypt verifier in ncgo's auth path purely for
  migrated rows; sentinel + forced reset is safer and the reset flow
  (`ncgo-cli user reset-password`) already exists.

### Wrapping the whole import in one transaction
- Pros: all-or-nothing.
- Cons: long-lived transactions against a production-sized source scan;
  a failure at row 900k loses everything. Per-entity commits plus
  idempotent skip-on-exists give resumability instead.

## Consequences

- `ncgo-cli import-nextcloud users` imports users, groups, and memberships
  from any supported source database into the configured ncgo database;
  the summary prints `users/groups/memberships: N created, M skipped
  (existing), K failed` plus capped warnings (20, then
  `... and N more warnings`).
- Migrated users with non-argon2id passwords must reset before first
  login; all users must re-login (no session import) and reissue app
  passwords.
- 4e2–4e4 reuse `importNCFlags`, `openSourceDB`, and `importReport`.
- ~~`oc_preferences` / `oc_accounts` (display-name metadata beyond
  `oc_users.displayname`, email, quota) are out of scope for v1 of the
  importer; email/quota follow-ups can extend the `users` subcommand.~~
  (**resolved by ADR-0071**: email and quota are mapped from
  `oc_preferences`/`oc_accounts`.)

## Verification

- `cmd/ncgo-cli/importnc_test.go`: source sqlite fixture (real argon2id
  hashes generated in-test, a literal `$2y$` bcrypt hash, an invalid uid,
  a uid_lower remap, a NULL displayname, orphan membership rows) imported
  into a migrated temp ncgo DB — argon2id hash byte-identical, bcrypt →
  sentinel, displayname fallback, invalid uid skipped with warning,
  remapped user warns and imports as `uid_lower`; imported argon2id user
  authenticates through `users.PasswordVerifier`; second run is all
  skipped; `--dry-run` counts match and leaves the target untouched;
  pre-existing target user is skipped and not overwritten; flag
  validation (driver/dsn/prefix) and unreachable-source errors.
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` no diff.
- Manual smoke run of the built binary: dry-run, real run, and idempotent
  second run against a temp sqlite source and target, all with expected
  output.
