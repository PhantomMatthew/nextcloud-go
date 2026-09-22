# ADR-0050: Phase 4e3 import-nextcloud shares

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 4e1 (ADR-0048) shipped the `import-nextcloud` scaffolding and `users`
subcommand; 4e2 (ADR-0049) added `files`. 4e3 adds the `shares` subcommand:
migrating internal (user/group) shares and public-link shares from the PHP
Nextcloud `oc_share` table into ncgo's `shares` table (migrations 0008/0009).
Unlike `files`, this subcommand *does* need the source database: shares are
purely database-resident metadata, and the shared path must be resolved
through `oc_filecache` (fileid → path) and `oc_storages` (which backend the
file lives on).

## Decision

1. **Join `oc_share` → `oc_filecache` → `oc_storages`, home storages only.**
   Each share's `file_source` fileid is resolved to a filecache `path`
   (`files/Documents/report.pdf`), and the filecache row's storage must be a
   home storage (`id` LIKE `home::%`). The leading `files/` is stripped to
   get the owner-relative path ncgo stores in `shares.file_path` (`files`
   itself maps to `/`). Shares on external/object storages (`local::…`,
   `object::…`, …) are skipped with a warning — ncgo's import (4e2) only
   migrates home-directory content, so the target file could not exist.
   Shares whose filecache row is missing (stale `oc_share` rows are common)
   are skipped with a warning, as are filecache paths outside the user's
   `files/` root.

2. **Share-type mapping.**

   | oc_share `share_type` | Meaning | ncgo mapping |
   |---|---|---|
   | 0 | user | `shares` row, `share_type=0`, `share_with=<recipient uid>` |
   | 1 | group | `shares` row, `share_type=1`, `share_with=<recipient gid>` |
   | 3 | public link | `shares` row, `share_type=3`, token verbatim |
   | 4, 6, 7 | remote/federated/circle | **skipped** with warning (counted under "remote shares") |

   ncgo's `files.ShareType*` constants deliberately carry the same numeric
   values (0/1/3), so supported types import without renumbering. Remote
   shares are deferred: they must be re-established over OCM after migration
   (ncgo's own OCM support landed in Phase 3d); importing their rows would
   create shares whose remote counterpart has never heard of ncgo.

3. **Public-link tokens are imported verbatim.** Existing shared URLs
   (`/s/<token>`) keep working across the migration — clients, emailed
   links, and embedded references do not break. ncgo's `shares.token` is
   `UNIQUE`, so a token that collides with an *unrelated* existing share is
   skipped with a warning (the pre-existing share is never overwritten). A
   collision with the *equivalent* share (same owner+path+type) is the
   idempotent re-run case and is skipped silently.

4. **Link-password policy: argon2id verbatim, anything else skips the share.**
   `$argon2id$…` PHC hashes (PHP `password_hash` output) are imported
   byte-identical and verify through ncgo's `Hasher.Verify` — the same
   compatibility proven for user passwords in 4e1. Any other hash format
   (bcrypt `$2y$…`, argon2i, legacy) causes the share to be **skipped**
   with a warning, never imported unprotected: silently dropping password
   protection on a shared link would be a security regression. Admins
   re-create those links after migration. Empty passwords stay empty.

5. **User/group shares get a fresh random token.** Nextcloud stores no token
   for internal shares, but ncgo's `shares.token` is `NOT NULL UNIQUE` and
   the sharing service generates a random 15-character
   `[a-z0-9]` token for every share type at create time. The importer does
   the same (the service's generator is unexported, so the ~10-line
   generator is duplicated in the CLI); on the astronomically unlikely
   token collision the insert is retried once with a new token.

6. **Field mapping.**

   | oc_share | ncgo `shares` | Notes |
   |---|---|---|
   | `uid_owner` | `owner_user_id` | resolved via users store; missing owner → skip+warn |
   | `share_with` | `share_with` | recipient uid/gid verbatim; missing recipient user/group → skip+warn |
   | filecache path | `file_path` | home-storage paths only, `files/` stripped |
   | (target filecache) | `item_type` | `folder` if the target filecache entry is a dir, else `file` |
   | `permissions` | `permissions` | **verbatim** — ncgo uses the identical bitmask (1=read, 2=update, 4=create, 8=delete, 16=share; verified in `internal/webdav/davutil.go`) |
   | `stime` (unix s) | `stime_ms` | ×1000 |
   | `expiration` | `expire_ms` | `YYYY-MM-DD HH:MM:SS` parsed as UTC; NULL → 0; unparseable → **skip** (importing without the expiry would silently extend the share's validity) |
   | `token` | `token` | link: verbatim; user/group: generated |
   | `password` | `password_hash` | see decision 4 |
   | `label` | `label` | verbatim (NULL → '') |
   | `note` | — | **dropped** (ncgo has no note field); one summary warning per run |
   | `uid_initiator` | — | **dropped** (ncgo tracks only the owner); documented here |
   | `file_target` | — | ignored — that is the recipient-relative mount name; ncgo mounts incoming shares by basename of the owner path |
   | — | `accepted` | 1 (ncgo default; NC auto-accepts in the versions we read) |

7. **Preconditions enforced per share, never fatal.** Owner must exist in the
   target users store ("run 'import-nextcloud users' first"), recipient
   user/group likewise, and the shared path must exist in the target
   filecache for the owner ("run 'import-nextcloud files' first"). Each
   violation skips the share with a warning; the run continues.

8. **Idempotency keys.** Links: existing equivalent share found by
   `GetByToken`. User/group: `ListByOwner(owner, path)` scanned for a row
   with the same `share_type` and `share_with`. Either way the share is
   counted skipped, so an interrupted run can simply be repeated. With
   `--dry-run` all checks and counts run against read-only stores and
   nothing is written.

9. **Reporting.** Per-category counters via the 4e1 `importReport`:
   `user shares`, `group shares`, `link shares`, `remote shares` (skipped
   only), each printed as `N created, K skipped (existing), J failed`;
   warnings capped at 20 printed plus a count of the rest, and a single
   summary warning when any `note` values were dropped.

## Alternatives Considered

### Importing remote/federated shares by re-notifying over OCM
- Pros: seamless federation migration.
- Cons: requires network access to every remote host during import, OCM
  handshake state machines, and failure modes (remote down, remote refuses)
  that a bulk import cannot resolve. Deferred; admins re-share over OCM.

### Generating fresh link tokens
- Pros: uniform token alphabet/length.
- Cons: breaks every existing shared URL — unacceptable continuity loss for
  a migration tool. Tokens are imported verbatim instead.

### Importing bcrypt link passwords with a bcrypt verifier
- Pros: those links keep their passwords.
- Cons: adds a bcrypt dependency (ncgo standardized on argon2id) for a
  transitional edge case. Skipping the share (with a loud warning) is safe
  and simple; the admin re-protects the link post-migration. Importing the
  row *without* the password was rejected outright (security regression).

### Skipping the filecache/storages join and trusting `file_target`
- Pros: simpler query.
- Cons: `file_target` is the recipient-relative mount path, not the owner's
  path; using it would corrupt `shares.file_path` and break incoming-mount
  resolution. The join is mandatory.

## Consequences

- `ncgo-cli import-nextcloud shares --source-driver … --source-dsn …`
  imports user, group, and link shares after `users` and `files` have run,
  printing the shared summary format.
- Existing public-link URLs survive the migration unchanged; argon2id
  password protection survives too.
- bcrypt-protected links, remote/circle shares, external-storage shares,
  and shares of files not yet imported are surfaced as explicit skips —
  never silent data loss, never weakened protection.
- Re-running the import is a no-op (all skipped); `--dry-run` writes
  nothing.
- Share `note` and initiator metadata are dropped (ncgo schema has no
  fields); the note drop is reported once per run.
- `dav` (4e4) follows and can assume users, files, and shares are in place.

## Verification

- `cmd/ncgo-cli/importnc_shares_test.go`: source fixture with all twelve
  categories (user share with note, group share on a folder, argon2id link
  with label+expiration, open link, bcrypt link, two remote types, missing
  filecache row, external storage, unknown recipient, unknown owner, target
  file missing) imported into a migrated temp sqlite + localfs target with
  alice/bob/team and DAV-created files — per-category counts and every
  warning asserted; rows verified through `SQLShareStore` read methods
  (permissions verbatim, stime→ms, item_type from the target filecache,
  generated 15-char tokens for internal shares, `accepted=1`); link token
  verbatim; the imported argon2id hash verifies via
  `Service.ResolvePublic` (and a wrong password fails); expiration
  `2030-01-02 03:04:05` UTC → exact ms; token collision with an unrelated
  share skips and leaves the pre-existing share untouched; `--dry-run`
  counts correctly and writes zero rows; second run all-skipped with a flat
  share count; `ncHomePath`/`ncExpirationMs` unit-tested including the
  date-only rejection.
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` no diff.
