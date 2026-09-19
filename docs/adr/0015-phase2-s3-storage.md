# ADR-0015: Phase 2g S3 Storage Backend

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2g S3 backend decisions relative to ADR-0014)

## Context

Phase 2 requires an S3-compatible object backend alongside localfs. Config already
has `storage.backends.*.type: s3` fields. `storage.Storage` is frozen; only
`localfs` implemented it. Filecache semantics stay path-keyed and backend-agnostic.

## Decision

1. **One client.** `github.com/minio/minio-go/v7` is the only S3 SDK.
2. **Package.** `internal/storage/s3.New(cfg config.BackendConfig) (storage.Storage, error)`
   implements `Stat/Open/Create/Delete/List/Rename/Mkdir`.
3. **Object keys.** `{uid}{normalizedPath}` — the same strings localfs already
   receives (`alice/hello.txt`). No leading slash. `..` and absolute paths are
   `ErrInvalidPath`.
4. **Directories.** Zero-byte marker objects at `path/`. `List` treats keys
   under a prefix as children (delimiter `/`). Implicit directories exist when
   any child key is present.
5. **Uploads.** `Create` buffers; `Close` calls `PutObject`. Bodies larger than
   8MiB use multipart (`PartSize` 8MiB).
6. **Wiring.** `app.openStorage` accepts `type == "s3"` and reads
   `endpoint, bucket, access_key_id, secret_access_key, region`. Endpoint URLs
   set TLS from the scheme; a host with no scheme defaults to HTTPS. Path-style
   bucket lookup is used so custom MinIO endpoints work.
7. **Tests.** Unit tests use an in-memory `objectAPI` (no network).
   `//go:build integration` talks to a real endpoint when `NCGO_S3_ENDPOINT`
   and `NCGO_S3_BUCKET` are set.

## Alternatives Considered

### AWS SDK v2
- Pros: first-party AWS support.
- Cons: the plan locked a single client; minio-go covers MinIO and AWS.

### Change filecache keys
- Pros: could store content hashes.
- Cons: DAV already addresses storage as `uid + path`.

## Consequences

### Positive
- Operators can point `default_backend` at S3 without changing WebDAV or
  filecache code.

### Negative
- Empty directories require marker objects. A crash after copy and before
  delete in `Rename` can leave two prefixes.

### Neutral / follow-ups
- Filename search and user/group shares remain later Phase 2 increments.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- ADR-0014
