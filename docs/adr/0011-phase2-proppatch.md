# ADR-0011: Phase 2d WebDAV PROPPATCH Favorites

- **Status**: Accepted
- **Date**: 2026-09-18
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2d PROPPATCH decisions relative to ADR-0010)

## Context

Desktop clients star files with PROPPATCH `{http://owncloud.org/ns}favorite`
and expect the same property on files PROPFIND. Phase 2a–2c return 405 for
PROPPATCH. Trash restore assigns a new filecache id, so favorite must not
be keyed by fileid.

## Decision

1. **PROPPATCH.** RFC 4918 `propertyupdate` on files DAV returns 207
   Multi-Status. Only `oc:favorite` with values `0` or `1` is persisted.
   Other names and protected live DAV properties return 403 inside 207.
2. **Path-keyed store.** Migration `0006_file_properties` table
   `file_properties` is unique on `(user_id, file_path, ns, name)` with no
   FK to `files`. MOVE/COPY/Purge/trash restore rename, copy, or delete
   rows by path, matching versions.
3. **PROPFIND.** Files and webdav-root namespaces always emit
   `<oc:favorite>0|1</oc:favorite>`. Uploads, trashbin, and versions do not.
4. **Capabilities.** No new `files.favorites` key.
5. **DAV class.** Compliance stays `1, 3, extended-mkcol` (LOCK is later).

## Alternatives Considered

### Add a `files.favorite` column
- Pros: simpler Stat.
- Cons: trash restore creates a new row and would drop the star.

### Persist arbitrary dead properties
- Pros: closer to Sabre `oc_properties`.
- Cons: no desktop client in this increment needs them; 403 keeps the
  wire surface small.

## Consequences

### Positive
- Desktop favorite toggle works across trash restore to the same or a new path.

### Negative
- Unknown PROPPATCH properties are 403 rather than stored.

### Neutral / follow-ups
- LOCK, sharing, S3, jobs, and search remain later Phase 2 increments.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- RFC 4918 PROPPATCH
- ADR-0010
