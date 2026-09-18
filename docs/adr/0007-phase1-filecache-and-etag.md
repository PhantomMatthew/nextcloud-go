# ADR-0007: Phase 1 Filecache, ETag, and WebDAV Root Alias

- **Status**: 🟢 Accepted
- **Date**: 2026-09-18
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 1 storage and property decisions relative to ADR-0005)

## Context

Phase 1 must let an existing Nextcloud desktop client log in and browse
`/remote.php/dav/files/{user}/` read-only. Phase 0 froze `storage.Storage` and
`webdav.FS` but served an in-memory filesystem. Capabilities already advertise
`webdav-root` as `remote.php/webdav`. ETag and `oc:id` / `oc:fileid` must be
stable across restarts without claiming byte-level identity with PHP
`oc_filecache`.

## Decision

1. **Filecache.** Metadata lives in SQL migration `0002_filecache` (`files`
   table). Bytes stay on `storage.Storage`; Phase 1 wires only `localfs`.
   Production `webdav.FS` is `internal/files.DAV`. `InMemoryFS` remains for
   handler unit tests.
2. **ETag.** File etag is `sha1(fileid || 0x00 || mtime_unix_ns || 0x00 || size)`
   hex. Directory etag is the same fingerprint over direct child etags sorted
   by path. Child changes recompute the ancestor chain to the user root. This
   is **not** PHP `oc_filecache` etag-byte compatible.
3. **Identifiers.** `oc:id` and `oc:fileid` stay `FileID(numericID, instanceID)`.
   `oc:owner-id` / `oc:owner-display-name` come from the path owner.
   `oc:checksums` is emitted only when `files.checksum` is non-empty
   (`SHA256:...` written on PUT/Write). `nc:is-encrypted` is `false`;
   `nc:mount-type` is empty. User-root PROPFIND emits `d:quota-used-bytes`
   (filecache usage) and `d:quota-available-bytes` (`-3` when
   `users.quota_bytes` is NULL).
4. **webdav-root alias.** `/remote.php/webdav/` mounts the same FS. The owner
   is `auth.Principal.UID`; the remainder of the URL is the relative path.
   Missing principal is 401. `/remote.php/dav/files/{user}/` still parses the
   user from the URL and rejects mismatches with 403.

## Alternatives Considered

### Reuse PHP `oc_filecache` schema and etag algorithm
- Pros: easier future `import-nextcloud`.
- Cons: greenfield schema already diverges; PHP etag internals are not a
  documented wire contract.

### Keep `InMemoryFS` in `app` until Phase 2
- Pros: less code in Phase 1.
- Cons: desktop browse cannot survive process restart; contradicts the
  accepted `00` Phase 1 goal.

## Consequences

### Positive
- DAV metadata and bytes survive `ncgo` restart on localfs.
- Capabilities `webdav-root` and files DAV share one adapter.

### Negative
- Directory etag is not PHP-identical; clients that cache etags against a PHP
  server will re-download after cutover.
- SHA-1 is used only as a non-cryptographic fingerprint.

### Neutral / follow-ups
- Chunked upload, PROPPATCH, LOCK, S3, and Sharing remain Phase 2.
- Desktop smoke and ≥50 HAR captures remain operator work.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 1
- ADR-0005 (auth strategy; session cookie and Bearer completed in this phase)
- `internal/files`, `internal/webdav`, `internal/migrations/sql/*/0002_filecache.*.sql`
