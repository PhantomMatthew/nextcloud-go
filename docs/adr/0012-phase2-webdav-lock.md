# ADR-0012: Phase 2e WebDAV Exclusive Write Locks

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2e LOCK decisions relative to ADR-0011)

## Context

Desktop and WebDAV clients issue RFC 4918 LOCK/UNLOCK and expect 423 Locked on
unlocked writes. Phase 2d advertised DAV class `1, 3` without LOCK. files_lock
OCS is a later increment.

## Decision

1. **Exclusive write locks only.** Files DAV (and webdav-root, same `files.DAV`)
   implement LOCK/UNLOCK. Shared `<d:shared/>` is 403. Uploads, trash, and
   versions do not implement `LockFS` and return 405.
2. **Path-keyed store.** Migration `0007_file_locks` table `file_locks` is unique
   on `(user_id, file_path)` and `token`, with no FK to `files`. `timeout_ms` is
   absolute expiry unix-ms. Lazy expire on Lock/CheckLock.
3. **Depth 0.** Missing Depth is 0. Depth `1` / `infinity` is 400. No lock-null:
   missing path is 404.
4. **Tokens.** `opaquelocktoken:` plus 32 hex chars. Timeout `Second-N` capped at
   86400; `Infinite` or omitted is 1800 seconds. Goldens freeze the token.
5. **Writes.** PUT, MKCOL, DELETE, MOVE, COPY, and PROPPATCH call CheckLock.
   GET/HEAD/PROPFIND/OPTIONS never 423. MOVE renames the lock row; COPY does not
   copy it. Trash Remove deletes the lock at the files path.
6. **DAV class 2.** OPTIONS `DAV:` is `1, 2, 3, extended-mkcol`. Files PROPFIND
   always emits `supportedlock` and `lockdiscovery`. No `files.locking` capability.

## Alternatives Considered

### files_lock OCS first
- Pros: matches Nextcloud Android extra APIs.
- Cons: desktop WebDAV LOCK is the RFC baseline; OCS stays later.

### Distributed lock manager
- Pros: multi-node.
- Cons: single-node SQL matches this increment's risk note.

## Consequences

### Positive
- Clients can lock a file and receive 423 without a matching If token.

### Negative
- Locks are not cluster-safe across multiple app nodes.

### Neutral / follow-ups
- files_lock OCS, shared locks, Depth infinity, lock-null, sharing, S3, jobs,
  and search remain later Phase 2 increments.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- RFC 4918 LOCK/UNLOCK
- ADR-0011
