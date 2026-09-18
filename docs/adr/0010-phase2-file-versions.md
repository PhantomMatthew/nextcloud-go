# ADR-0010: Phase 2c File Versions

- **Status**: 🟢 Accepted
- **Date**: 2026-09-18
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2c versioning decisions relative to ADR-0009)

## Context

Phase 2a/2b overwrite a live file in place. Desktop clients list history at
`/remote.php/dav/versions/{user}/versions/{fileid}` and roll back by MOVE
into `/restore` when `files.versioning` is true.

## Decision

1. **Snapshot on overwrite.** `DAV.Write` copies the previous bytes into
   `file_versions` before replacing content. First PUT does not snapshot.
2. **Path-keyed history.** Rows are keyed by `(user_id, file_path, revision)`
   with no FK to `files`, so trash restore (new fileid, same path) still
   finds versions. Bytes live at `versions/{uid}/{row_id}`.
3. **Revision names** are the previous file `Mtime` unix seconds, with `-n`
   on collision.
4. **Restore.** MOVE from `/versions/{fileid}/{revision}` to the versions
   `/restore` collection snapshots the current file then copies the
   revision over it.
5. **Assemble checksum mismatch** uses `RollbackLatest` (restore newest
   snapshot without another snapshot, then delete it) instead of Purge.
6. **Capabilities.** `files.versioning` is `true`.
7. **Expiry.** Default 30 days, lazy on List of that file's versions.

## Alternatives Considered

### Key versions by filecache id
- Pros: rename is free.
- Cons: trash restore assigns a new id and would orphan history.

### Expire via jobs
- Pros: bounded background work.
- Cons: jobs Runner is still unimplemented.

## Consequences

### Positive
- Desktop version listing and rollback match Nextcloud URLs.

### Negative
- Lazy expiry only runs when versions are listed.

### Neutral / follow-ups
- PROPPATCH, LOCK, sharing, S3, jobs, and search remain later Phase 2
  increments.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- Nextcloud developer manual: Versions WebDAV
- ADR-0009
