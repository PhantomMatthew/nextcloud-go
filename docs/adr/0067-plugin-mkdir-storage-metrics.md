# ADR-0067: Plugin storage_mkdir host function + per-plugin storage byte metrics

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0042 (its "mkdir host function" and "per-plugin storage
  metrics" follow-ups are now closed), ADR-0061 (the same two follow-ups
  closed; the chunked/resumable write follow-up is evaluated and deferred
  below)

## Context

ADR-0042 shipped the streaming storage ABI (`storage_open/create` plus the
stream functions, `delete/list/rename`) and ADR-0061 added quotas; both
registered the same three follow-ups. Two of them are small and belong to
the same storage surface, so they land together:

1. **A `storage_mkdir` host function.** Plugins creating directory trees
   (state dirs, cache layouts) must today create a placeholder file and
   delete it, or write every file with a fresh path prefix — `storage_create`
   materializes parents implicitly, so a plugin *can* survive without mkdir,
   but it can never make an empty directory and every tree is an accident of
   its writes.
2. **Per-plugin storage byte metrics.** The §12 families count calls and
   latency but not bytes; a plugin quietly moving gigabytes through
   `storage_stream_read`/`write` is invisible next to one moving kilobytes.
   Bytes are the unit the storage quotas (ADR-0061) already think in.

The third follow-up — chunked/resumable plugin writes — is evaluated and
deferred below.

Spec §9 explicitly allows new host functions within a major ("New host
functions added within a major are OK; missing functions trap if called"),
so the ABI stays `ncgo-abi/1`.

## Decision

1. **`storage_mkdir(path_ptr, path_len) -> i32`** (module `ncgo`):
   - Resolution reuses `resolveStorage(..., write=true)` unchanged: scheme
     prefix, normalization/jail, and the write-grant check (user scope needs
     `storage.write` with "user", system scope with "system") are exactly the
     write path's.
   - User scope commits through `files.DAV.Mkdir` (filecache-consistent,
     incoming-mount aware, like every other user-scope write). System scope
     calls `SystemStorage.Mkdir` on the resolved in-jail path.
   - **Single-level semantics: no implicit parents.** An existing target maps
     to `ErrCodeAlreadyExists` (-5) in both scopes; a missing parent maps to
     `ErrCodeNotFound` (-4). To make the user scope's answer match the
     system scope's, `localfs.Mkdir` now maps a missing parent to the
     `storage.ErrNotFound` sentinel (via the file's existing `mapExistErr`
     helper, as `Stat`/`Open`/`Delete` already do) instead of leaking a raw
     `*PathError` that only the default — log-and-internal (-1) — branch of
     the ABI error mapping could catch. The system tree root is the plugin's
     jail and conceptually always exists, but backends materialize it lazily:
     `storage_create` builds it implicitly, while `storage_mkdir` under a
     never-written root answers -4 until the first write lands. This mirrors
     mkdir(2) closely enough that plugin authors should find no surprises.
   - Directories carry no bytes, so neither the user quota nor the system
     tree quota (ADR-0061) applies.
2. **Byte metric family `ncgo_plugin_storage_bytes_total{plugin, op,
   scope}`** with `op ∈ read|write` and `scope ∈ user|system`, registered in
   `observability.NewRegistry` as the fourth plugin family. The Registry
   gains `AddCounter(name, delta, labels...)` next to `IncCounter` (which now
   delegates); non-positive deltas are no-ops so counters stay monotonic and
   zero-byte transfers create no empty series. Count points sit where the
   bytes actually move, never at the ABI entry:
   - user writes: `commitSpool` after a successful DAV commit (the actual
     spooled bytes, not the declared size);
   - system writes: `commitSystemWrite` after a successful close *and* quota
     pass (a quota-refused commit is rolled back and counts nothing);
   - reads: `storageStreamRead` per actual read; the open stream handle now
     carries its scope (`storageReadStream`), recorded at `storage_open`
     time, so reads need no re-resolution.
   A nil registry skips counting entirely (the 4i zero-overhead posture), and
   the plugin label follows the 4n/4o nil-guard (no call context → empty id,
   still counted).
3. **Chunked/resumable plugin writes are deferred, conditionally.** The
   deferral is a value/ABI-surface call, not an oversight:
   - The write spool already covers a single file up to 1 GiB
     (`HostConfig.MaxSpoolBytes`), and ADR-0061 closed the quota hole on both
     scopes, so the spool is not a bypass risk that chunking would fix.
   - Plugin storage holds state/config/cache-shaped data — kilobytes to low
     megabytes in practice. The DAV chunked-upload machinery (Phase 2,
     `Uploads.MkdirMeta` et al.) exists for human clients pushing large
     media through flaky connections; plugins are server-local callers with
     no flaky hop between guest and host.
   - A plugin-side chunked ABI means at minimum `upload_init`/`upload_chunk`/
     `upload_finish` (or a resumable create), plus host-side chunk assembly
     state, expiry for abandoned sessions, and quota accounting across
     sessions — three or more new functions and a state machine to carry
     forever, for a need no concrete plugin has.
   - **Reopen condition**: a real plugin that must move blobs near or beyond
     the spool cap (e.g. a backup/migration plugin), or an operator report of
     spool-cap pressure, reopens this — the design then starts from the
     Phase 2 chunked-upload spec rather than a new mechanism.
4. **SDK:** `pluginsdk.StorageMkdir(path string) int32` (wasm binding plus
   non-wasm stub returning `ErrCodeUnsupported`), matching the existing
   storage binding pairs.

## Consequences

- Plugins can build empty directory trees in both scopes with the same
  capability grants they already hold for writes; no new capability key.
- Storage-heavy plugins become visible in the §12 exposition in the same
  unit the quotas enforce; the four label values per series stay bounded
  (op and scope are two-value enumerations), so the cardinality budget is
  unchanged in kind.
- Read streams are wrapped in a `storageReadStream` (scope tag): passing a
  request-body handle to `storage_stream_read`, which the old
  `io.ReadCloser` assertion accepted, now answers `ErrCodeInvalidArgument` —
  a strictness improvement, and no behavior the spec promised.
- The `localfs.Mkdir` sentinel fix also reaches the DAV's WebDAV surface:
  MKCOL with a missing parent previously returned 500 (raw `*PathError`) and
  now returns 404 (`webdav.ErrNotFound`). RFC 4918's 409 for that case
  remains a separate gap — the filecache's `ErrParentMissing` mapping is only
  reached when the backend create already succeeded.
- A guest compiled against the 4u SDK running on a pre-4u host fails to
  instantiate (the import is absent) — the standard §9 posture for new
  functions, same as 4s/4t.
