# ADR-0049: Phase 4e2 import-nextcloud files from a Data Directory

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 4e1 (ADR-0048) shipped the `import-nextcloud` scaffolding and the
`users` subcommand. 4e2 adds the `files` subcommand: migrating users' actual
file trees from a PHP Nextcloud **data directory** into the configured ncgo
storage backend and filecache. This is the largest and most valuable part of
a migration, and the one where a wrong ingest path silently corrupts the
filecache invariants (etags, checksums, ancestor sizes, version snapshots).

## Decision

1. **Walk the data directory, not the filecache table.** The source
   database flags (`--source-driver`/`--source-dsn`) are *not* used by this
   subcommand — the parent's persistent-flag validation is overridden by the
   subcommand's own `PersistentPreRunE`, which validates `--datadir`
   instead. Nextcloud's `oc_filecache` is explicitly a cache: it can be
   stale, incomplete, or rebuilt by `occ files:scan`, while
   `<datadir>/<uid>/files/` is canonical for content. Walking the filesystem
   also makes the import work for instances whose database is already gone
   and avoids any filecache-schema-version coupling. Filesystem mtimes
   (truncated to second resolution, matching Nextcloud's unix-second
   filecache mtimes) become the ncgo file mtimes.

2. **`DAV.Write` is the ingest pipeline.** Files are ingested through
   `files.DAV.Write` and directories through `DAV.Mkdir`, with the DAV wired
   exactly as production wires it in `app.New` (storage backend, filecache
   store, properties, locks, trash, versions) so imported data gets the same
   invariants a client upload gets: SHA256 checksums, etag computation,
   ancestor size/mtime recalculation, filecache bookkeeping, and version
   snapshots on overwrite. Shares/incoming/remote/events stay nil (the write
   path nil-guards them). The storage backend is opened from the server
   config by a small `openStorage` helper duplicated into `cmd/ncgo-cli`:
   the switch cannot live in `internal/storage` without an import cycle
   (the localfs/s3 backends import the storage package), and importing
   `internal/app` into the CLI would drag in the HTTP/plugin stack for one
   function.

3. **Skip-if-unchanged idempotency.** Before writing, the target filecache
   is checked read-only (`Meta.GetByPath` — deliberately *not* `DAV.Stat`,
   which lazily creates the user's storage home and filecache root as a
   side effect, a write `--dry-run` must not perform). A target file with
   the same size and mtime (second resolution) is skipped. This makes
   re-runs cheap and, crucially, avoids a spurious version snapshot per file
   on every repeated import (`DAV.Write` snapshots the previous version on
   overwrite, exactly like a client PUT). A file that differs is rewritten
   and counted as updated, and its previous content lands in `file_versions`
   — the same behavior a desktop-client sync would produce.

4. **Nextcloud-internal directories are never descended into.** Only
   `<datadir>/<uid>/files` is walked; the sibling `files_trashbin`,
   `files_versions`, `uploads`, `cache`, and `thumbnails` directories are
   therefore excluded by construction (trash and versions are re-created by
   ncgo's own subsystems going forward, not migrated). Top-level entries are
   treated as user dirs only when they contain a `files/` subdirectory and
   are not `appdata_*` (which can contain a literal `files/` subdir from
   app data). Dotfiles inside the tree are real user files and are
   imported. Symlinks are never followed (skipped with a warning), and
   non-regular special files are skipped with a warning.

5. **No server-side encryption support.** A user with a
   `<uid>/files_encryption` directory is skipped wholesale with the warning
   "server-side encrypted source not supported, user skipped". ncgo has no
   server-side encryption subsystem to import keys into, and importing the
   ciphertext would produce unreadable garbage; the admin must decrypt the
   source (or migrate that user's data out-of-band) first.

6. **Best-effort, resumable, per-file semantics.** Writes commit per file
   (DAV autocommit), per-file errors increment the failed counter, warn, and
   continue; an interrupted run is simply repeated and the skip-if-unchanged
   check makes the re-run a no-op for everything already imported. A failed
   directory skips its subtree. Unknown target users are skipped with a
   warning pointing at `import-nextcloud users` (the precondition). With
   `--dry-run`, the walk and all skip/update accounting run against the
   read-only filecache and nothing is written. Progress is one line per
   user; `--verbose` adds a line per file.

## Alternatives Considered

### Reading `oc_filecache` and streaming from the source DB
- Pros: no filesystem access needed; could work remotely.
- Cons: filecache is a cache — path/size/mtime may not match the bytes on
  disk; storage IDs must be resolved to actual backends; encrypted/external
  storages complicate row interpretation; couples the importer to the
  filecache schema of every supported Nextcloud version. The datadir walk is
  simpler and canonical.

### Bulk-loading the filecache directly (bypassing DAV.Write)
- Pros: faster for very large trees.
- Cons: duplicates etag/checksum/ancestor-recalc/version logic in a second
  implementation that can drift from production; correctness beats import
  throughput for a one-time operation.

### Importing trashbin and versions
- Pros: fuller migration fidelity.
- Cons: both are derived data; their directory layouts carry no metadata
  (deletion times live in oc_files_trash, version metadata in oc_filecache
  rows we'd have to trust), and ncgo regenerates both through normal use.
  Deferred; can be added later behind flags if admins ask.

## Consequences

- `ncgo-cli import-nextcloud files --datadir /path/to/data` imports all
  discovered users (or `--user uid`, repeatable), printing per-user progress
  and the shared summary (`users/files/directories: N created[, M updated],
  K skipped (existing), J failed`).
- Re-running the import is a no-op scan (all skipped) unless source files
  changed; changed files are updated with the old content preserved as a
  version.
- Migration order matters: `users` before `files`; `shares` (4e3) and `dav`
  (4e4) follow and can assume files are in place.
- Encrypted instances, external-storage mounts, trash, and versions are out
  of scope; each is surfaced as an explicit skip/warning, never silent data
  loss.

## Verification

- `cmd/ncgo-cli/importnc_files_test.go`: temp datadir fixture (nested dirs,
  dotfile, `files_trashbin`/`files_versions`/`cache` siblings, unknown user,
  encrypted user, `appdata_*` with a `files/` subdir, symlink) imported into
  a migrated temp sqlite + localfs target — content byte-identical in the
  storage backend, mtimes preserved at second resolution, internal siblings
  and non-user dirs absent, `.hidden` imported, unknown/encrypted users
  warned and skipped, symlink warned and skipped; second run all-skipped
  with a flat `file_versions` count; size-changed file updated with exactly
  one new version row; `--dry-run` counts correctly and writes nothing (no
  storage dirs, empty filecache); `--user` filter; flag validation.
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` no diff.
