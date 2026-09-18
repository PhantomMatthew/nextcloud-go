# ADR-0008: Phase 2a Chunked Upload v2

- **Status**: 🟢 Accepted
- **Date**: 2026-09-18
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2a write-path and chunking decisions relative to ADR-0007)

## Context

Phase 1 persisted files through `files.DAV` and localfs, but desktop bidirectional
sync also needs chunked upload NG: capability `dav.chunking >= 1.0` and the
collection `/remote.php/dav/uploads/{user}/{transferId}/`. `PUT` with
`OC-Chunked` remains the legacy v1 protocol. MOVE of `uploads/.../.file` targets
`/remote.php/dav/files/...`, which is outside the uploads handler prefix.

## Decision

1. **Writes.** Existing `PUT`/`MKCOL`/`DELETE`/`MOVE`/`COPY` on `files.DAV` stay
   the production write path. DELETE is still permanent (trash is a later
   increment). `Handler.put` streams `r.Body` into `FS.Write` with
   `io.LimitReader` when `Content-Length` is known.
2. **Chunked upload v2.** Migration `0003_uploads` stores transfer sessions.
   Chunk bytes live at `uploads/{uid}/{transferId}/{chunkName}` on
   `storage.Storage`, not in the `files` table. Chunk names are digits only;
   5-digit and 6-digit padding are both accepted and ordered numerically.
3. **Assemble.** `webdav.Handler.Assemble` intercepts `MOVE` of
   `/{transferId}/.file` and concatenates consecutive chunks from 1 into
   `files.DAV.Write`. Destination is parsed against `/remote.php/dav/files/`.
   Assembly is synchronous (no `202` / `OC-JobStatus-Location`).
4. **Capabilities.** `dav.chunking` is `"1.0"`. `files.chunked_upload` advertises
   `max_size=5368709120` and `max_parallel_count=20`.
5. **Legacy.** `OC-Chunked` on files PUT remains `501`.

## Alternatives Considered

### Implement OC-Chunked v1 instead of NG
- Pros: fewer new routes.
- Cons: current desktop clients use NG when `dav.chunking >= 1.0`.

### Return 202 and assemble in jobs
- Pros: large files do not block the request.
- Cons: `internal/jobs` is unimplemented; clients accept synchronous 201/204.

## Consequences

### Positive
- Desktop clients can discover chunking and upload large files in pieces.
- Filecache write goldens lock PUT/MKCOL/DELETE/MOVE behaviour.

### Negative
- Huge uploads occupy one request until concatenation finishes.
- Permanent DELETE will surprise clients that expect trashbin restore.

### Neutral / follow-ups
- PROPPATCH, LOCK, trash, versions, sharing, S3, jobs, and search remain later
  Phase 2 increments. Desktop smoke and HAR capture remain operator work.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- `docs/CHUNKED_UPLOAD_V2_SPEC.md`
- ADR-0007
