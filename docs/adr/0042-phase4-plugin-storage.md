# ADR-0042: Phase 4c4 Plugin Storage Host Functions

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Spec §6.3 defines the streaming `ncgo.storage_*` function family
(`storage_stat/open/create/stream_read/stream_write/stream_close/delete/
list/rename`) gated by `storage.read`/`storage.write` scopes (`"user"` /
`"system"`). Phase 4a registered them as stubs returning `ErrUnsupported`
after the capability check. This increment lands the real implementation:
user-scope file access through the files DAV, system-scope access through
the default storage backend under a per-plugin namespace, and streaming
read/write handles with commit-on-close semantics for user writes.

## Decision

1. **Scheme-prefix path routing (v1 convention).** Plugin storage paths
   carry a scheme: `user:/docs/x.pdf` addresses the calling user's files,
   `system:/conf.json` addresses the plugin's system storage. Bare paths
   (no scheme) default to `user:`. After the scheme is stripped, paths are
   canonicalized with `files.NormalizePath` — `..`, empty segments, NUL,
   and backslash weirdness are rejected with `ErrInvalidArgument`, as are
   unknown schemes. `/` (the scope root) is valid only for `storage_stat`
   and `storage_list`; create/open/delete/rename reject it with
   `ErrInvalidArgument`. `storage_rename` requires both paths in the same
   scope.

2. **User scope goes through the DAV; writes spool, then commit
   one-shot.** All user-scope operations run as the call's user
   (`CallContext.UserID`; empty → `ErrCodeUnavailable`) against the shared
   `files.DAV`, so reads see the filecache and writes stay consistent with
   it (etag, size, ancestors, versions, trash). Because `DAV.Write` is a
   one-shot commit, `storage_create` on a user path returns a handle to a
   temp-file spool (`os.CreateTemp`); `storage_stream_close` seeks to the
   start and commits the spooled content through `DAV.Write`, then removes
   the temp file. The commit runs under a fresh context built from the
   identity captured at create time (user + plugin), since the original
   call context may be gone. Spools are capped at 1 GiB
   (`HostConfig.MaxSpoolBytes`, `ErrCodeTooLarge` beyond). Leaked spools
   are discarded by `closeAll` — never committed. `storage_delete` moves
   to trash via `DAV.Remove`; `storage_rename` is `DAV.Move` with
   overwrite.

3. **System scope is namespaced appdata.** System paths resolve to
   `<SystemPrefix>/<plugin_id>/<cleaned path>` on the default storage
   backend, where app.go passes `SystemPrefix =
   "appdata_<instanceID>/plugins"` (Nextcloud's appdata convention). One
   plugin cannot see another plugin's tree. `storage_create` maps directly
   to `Storage.Create` (the backend's atomic temp+rename), reads to
   `Storage.Open`, delete/rename/list/stat to their `Storage` counterparts.
   Nil `Files` → user scope returns `ErrCodeUnavailable`; nil
   `SystemStorage` → system scope returns `ErrCodeUnavailable`.

4. **Granular capability checks.** The manifest's `storage.read` /
   `storage.write` scope lists are checked per operation: reads require
   `"user"`/`"system"` in `storage.read`, writes in `storage.write`
   (`ErrCodePermissionDenied` otherwise). A plugin with only `"user"` read
   cannot stat system paths even if it holds `"system"` write.

5. **Streams share the handleStream budget (64).** Open/create handles are
   registered in the caller's per-instance handle table under
   `handleStream`; exceeding the budget returns `ErrCodeUnavailable`. The
   table's `closeAll` releases leaked handles on every release path: read
   streams and system writers are closed, user spools are closed and their
   temp files removed. Error mapping mirrors `abi_db.go`:
   not-found → `ErrCodeNotFound`, exists → `ErrCodeAlreadyExists`,
   forbidden/invalid → `ErrCodePermissionDenied`, is-dir/not-dir →
   `ErrCodeInvalidArgument`, everything else `ErrCodeInternal` with a warn
   log.

6. **Event-driven calls get a user identity.** `events.Event` gains
   `UserID`; `DAV.emitUploaded` sets it from the writing user. The plugin
   dispatcher adopts it as the delivery's `CallContext.UserID` unless the
   publish context already carries explicit call metadata (explicit
   `WithCallContext` wins). Event-subscribed plugins can therefore use
   `user:` storage for the uploading user, while deliberate dispatch
   (route handlers, admin calls) keeps full control.

7. **Wiring.** `HostConfig` gains `Files *files.DAV`, `SystemStorage
   storage.Storage`, `SystemPrefix`, `MaxSpoolBytes`. The plugin-host
   block in `app.go` moves after the instanceID assignment (it only needs
   cache/DB/bus/registry/files/storage) so `SystemPrefix` can embed it.
   pluginsdk gains `StorageStat/List/Open/Create/Delete/Rename` plus
   `StorageStream.Read/Write/Close` bindings and the `StorageFileInfo`
   MessagePack shape (`{path, size, mtime_unix_ms, is_dir}`), with
   non-wasm stubs.

## Alternatives Considered

### Direct storage-backend access for user scope
- Pros: no DAV dependency in the plugin host; marginally less copying.
- Cons: bypasses the filecache — plugin writes would be invisible to
  `dav.Stat`/`dav.List`, etags would go stale, versions/trash would be
  skipped, and reads could disagree with metadata. Consistency with the
  rest of the server outweighs the indirection.

### Streaming commit (write-through) instead of spool
- Pros: no 1 GiB temp-file ceiling; constant memory/disk for huge uploads.
- Cons: `DAV.Write` is one-shot by design (single filecache transaction);
  a write-through stream would need chunked-upload machinery (transfer
  folders, assembly) that does not exist yet for this path. Spooling keeps
  v1 simple and atomic; chunked/resumable writes are a follow-up.

### Shared system namespace (no per-plugin prefix)
- Pros: plugins could exchange files through system scope.
- Cons: any plugin with a system grant could read/overwrite every other
  plugin's state; namespacing matches Nextcloud's per-app appdata layout
  and the default-deny posture. Cross-plugin sharing can be added later as
  an explicit capability.

## Consequences

- User writes are bounded by the 1 GiB spool cap and the temp filesystem;
  chunked/resumable upload support is follow-up work.
- No quota enforcement yet — plugin writes to user scope bypass the user's
  quota check (DAV.Write has none either); quota checks are a follow-up.
- Event-driven deliveries now carry a user identity; plugins should treat
  `user:` paths as "the user the event is about", which may be absent.
- Follow-ups: chunked/resumable writes, quota checks, per-plugin storage
  metrics (spec §12), `mkdir` host function (currently only writable via
  DAV parents created out-of-band for user scope; system scope creates
  parents implicitly).

## Verification

- Capability matrix through wasm guests: no grant → -3 on stat/create/
  delete/rename (both scopes), user-read-only allows stat/open/list but
  denies writes, system grants analogous, unknown scheme → -2, `..` → -2,
  empty UserID → -12, nil Files → -12 (user), nil SystemStorage → -12
  (system).
- End-to-end through wasm guests for both scopes:
  create → write → close(commit) → stat(size) → open → read(content
  match) → list(contains) → rename → stat new name → delete → stat gone
  (-4); user-scope content asserted on the localfs under `<uid>/...` and
  visible to a second guest through the DAV; system content asserted under
  `<SystemPrefix>/<plugin_id>/...`; plugin B cannot stat plugin A's system
  tree (-4).
- Stream budget: the 65th open returns -12; closeAll discards a leaked
  read handle + spool (temp removed, no commit); oversize spool write →
  -11; event with UserID enables the subscriber's `user:` stat, explicit
  call context on the publish ctx wins over the event UserID.
- `go test ./...` green, race clean, golangci-lint 0 issues.
