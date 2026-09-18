# ADR-0009: Phase 2b Trashbin

- **Status**: 🟢 Accepted
- **Date**: 2026-09-18
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2b trashbin decisions relative to ADR-0008)

## Context

Phase 2a DELETE on `files.DAV` permanently removed bytes and filecache rows.
Desktop clients treat DELETE as recoverable when `files.undelete` is true and
expect `/remote.php/dav/trashbin/{user}/`. MOVE/COPY overwrite must not fill
the trash with every sync replacement.

## Decision

1. **HTTP DELETE on files** calls `files.Trash.MoveToTrash`. Bytes move to
   `trash/{uid}/{location_id}` and a `trash_items` row records
   `original_path`. `location_id` is `{basename}.d{unix_seconds}` with `-n`
   on same-second collision.
2. **Permanent delete.** `DAV.Purge` is the previous hard-delete
   implementation. MOVE/COPY overwrite and checksum-mismatch assemble use
   Purge. DELETE on a trash item calls `PurgeLocation`.
3. **Restore.** `Handler.Restore` intercepts MOVE from
   `/trash/{location_id}` (or `/restore/{location_id}`). Destination under
   `/remote.php/dav/files/` restores to that path; destination under the
   trashbin restore collection uses `original_path`.
4. **Capabilities.** `files.undelete` is `true`. `files.versioning` is not
   advertised.
5. **Expiry.** Default retention is 30 days. `List`/`Stat` of trash items
   lazily purges rows whose `deleted_ms` is older than the cutoff. The jobs
   Runner is not used.

## Alternatives Considered

### modules/files-trash as a separate package
- Pros: matches a later module layout.
- Cons: Phase 2a already kept uploads in `internal/files`; splitting now
  would add import cycles with DAV.

### Expire via jobs
- Pros: bounded background work.
- Cons: `internal/jobs` Runner is still an empty interface.

## Consequences

### Positive
- Desktop DELETE/restore matches Nextcloud trashbin URLs and properties
  `oc:trashbin-original-location` and `oc:trashbin-deletion-time`.

### Negative
- Lazy expiry only runs when trash is listed; abandoned items linger until
  then.

### Neutral / follow-ups
- Versions, PROPPATCH, LOCK, sharing, S3, jobs, and search remain later
  Phase 2 increments.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- ADR-0008
