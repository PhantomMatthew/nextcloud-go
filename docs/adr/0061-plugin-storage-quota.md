# ADR-0061: Phase 4p Plugin Storage Quotas

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none) — closes the ADR-0042 "quota checks" follow-up

## Context

ADR-0042 gave the `storage_*` host functions two write paths: user scope
spools to a temp file and commits through `DAV.Write` (filecache-consistent,
one-shot), and system scope writes directly into the plugin's
`<SystemPrefix>/<id>/...` tree on `SystemStorage`. Neither path enforced any
quota. A plugin with `storage.user.write` could grow a user's storage
without bound — the core DAV itself does not enforce `users.quota_bytes`
either — and a plugin with `storage.system.write` faced only the per-file
1 GiB spool cap, with no bound on the number of files in its tree. This ADR
closes both holes for the plugin paths; the remaining ADR-0042 follow-ups
(chunked/resumable writes, per-plugin storage metrics, a mkdir host
function) stay open.

## Decision

1. **Two checkpoints per scope.** Quotas are checked at `storage_create`
   against the *declared* size (early refusal when the guest knows the size,
   which is the common case for config-shaped writes) and again at commit —
   `storage_stream_close` — against the *actual* bytes: the spool counter for
   user scope, a counting writer wrapper (`systemQuotaWriter`) for system
   scope. A declared size `<= 0` means "unknown" and skips the create-time
   check; the commit backstop always applies.

2. **Overwrite delta semantics.** Refusing condition in both scopes is
   `usage - oldSize + newBytes > quota`, where `oldSize` is the size of the
   pre-existing target (0 when absent — `DAV.Stat` / `SystemStorage.Stat`
   with `ErrNotFound` mapped to 0). Overwriting a file therefore pays only
   the growth delta, not the full rewrite, matching operator expectation
   that rewriting a same-size file never trips the quota.

3. **User scope uses user data, not config.** The quota is
   `users.User.QuotaBytes` (**nil = unlimited**) and the usage is the
   filecache `SUM(size)` already exposed as `files.Store.Usage`; `files.DAV`
   gains a `Usage(ctx, user) (int64, *int64, error)` pass-through pairing
   the two (resolveUser → Meta.Usage). No new config key: quota management
   stays on the existing user surface (`ncgo-cli user add/reset`).

4. **System scope gets a host-level cap.** `HostConfig.PluginSystemQuotaBytes`
   (`<= 0` selects the default 1 GiB) bounds the total byte size of one
   plugin's tree. The tree size is computed by walking `SystemStorage.List`
   recursively and summing `FileInfo.Size` (`systemTreeUsage`, deleteTree-style
   with the shared depth/entry safety bounds — List is one level deep).
   Plugin trees are quota-bounded and small, so recomputing per create is
   acceptable and no cached accounting is introduced. The walk runs once at
   create; the captured (tree, oldSize) pair feeds both the declared-size
   check and the commit backstop.

5. **Refusal posture.** Over-quota returns `ErrCodeQuotaExceeded` (-8) plus a
   warn log naming the plugin id (nil-guarded `callFromCtx`), scope, path,
   and quota/usage/write numbers — the 4n/4o posture. No new metric
   families: -8 classifies into the existing bounded `result` label. Quota
   *denials* are emitted by the checkpoints directly, never via
   `mapStorageErr`, so a refusal is never flattened into `ErrCodeInternal`;
   conversely a backend failure during a quota *lookup* maps through
   `mapStorageErr` (internal), failing closed rather than waving the write
   through.

6. **Refusal cleanup.** A user-scope refusal discards the spool — no bytes
   ever reach the DAV. A system-scope commit refusal arrives after the
   backend create has already committed (backend creates are atomic: localfs
   stages a temp file and renames on Close, and the storage interface has no
   abort), so the refusal deletes the just-written target on a best-effort
   basis; when the write overwrote an existing file, the old content was
   already replaced at Close and the delete only reaps the new content.

7. **Configuration.** `plugin.system_storage_quota_mb` (default 1024, 0 =
   host default, negatives rejected) joins the Plugin config section and is
   passed through by both `internal/app` and the `ncgo-cli` install host —
   install/upgrade hooks can write system storage and must not bypass the
   quota. There is no "unlimited" setting for the system tree, matching the
   default-deny posture; the user scope keeps its per-user nil = unlimited.

### Why the system default is 1 GiB

The per-file spool cap is already 1 GiB, so any single write can legitimately
be that large; a tree default below it would make one maximum-size file
unstorable, above it buys nothing for the config/cache-shaped data plugin
trees hold. 1 GiB exactly matches the spool cap — one maximum-size file
always fits — while closing the unbounded-file-count hole. Operators with a
legitimate larger plugin raise `plugin.system_storage_quota_mb`.

## Alternatives Considered

### Per-write enforcement instead of a commit backstop
- Pros: refuses the crossing byte before it reaches the backend; no
  delete-after-commit cleanup.
- Cons: changes the failure surface from "close returns -8" to "some write
  returns -8 and close still commits a truncated file if the guest proceeds"
  — a quieter corruption mode. The commit refusal keeps the spool/atomic-
  rename semantics: either the file lands whole or not at all.

### Persistent per-plugin byte accounting
- Pros: O(1) checks; no tree walk.
- Cons: a new counter to keep consistent across renames, deletes, and crash
  windows duplicates the filecache's job for user scope and invites drift
  for system scope. Recomputing small trees at create time is simpler and
  self-healing.

### Enforcing user quota inside DAV.Write itself
- Pros: covers the WebDAV/API surface too, not just plugins.
- Cons: core DAV quota semantics (trashbin/versions accounting, chunking,
  shared-file attribution) are their own design problem — out of scope for
  the plugin increment. The plugin path now checks *before* calling
  `DAV.Write`, which remains quota-free for other callers; this asymmetry
  (plugin writes quota-checked, core DAV writes not) is deliberate and
  tracked as a follow-up.

## Consequences

- ADR-0042's follow-up list is now: chunked/resumable writes (ABI
  extension), per-plugin storage byte metrics, a mkdir host function, and
  quota enforcement in the core DAV write path itself.
- Both checks are **best-effort**: the check and the commit are not
  transactional, so concurrent writes (two plugin instances, or a plugin
  and a WebDAV client in the user scope) can each pass the check and
  together exceed the quota. `DAV.Write` has no transactional quota either;
  closing that race belongs with the core-DAV-quota follow-up.
- The system-scope commit refusal deletes the target: a guest that ignores
  the -8 still loses the partial file, and an overwrite refusal loses the
  old content (already replaced at Close). Guests must treat -8 from
  `storage_stream_close` as "the write did not happen".
- The wasm-plugin-abi Change Log records the semantics; the config key
  appears in the Phase 0 blueprint YAML block.

## Verification

- `systemTreeUsage` unit tests: nested tree summation, empty tree, missing
  root (not an error), missing target (`old = 0`), directory target
  (`old = 0`).
- Wasm integration (real DAV + sqlite + localfs, wasmgen probes): user scope
  — create-time refusal on declared size (-8 + warn naming the plugin),
  commit-time refusal on actual spooled bytes (-8, no filecache entry, no
  bytes on disk), nil quota passes even an absurd declared size, overwrite
  pays only the delta (10-byte file, quota 15, 12-byte rewrite succeeds;
  16-byte rewrite refuses and leaves the old file). System scope —
  create-time refusal counting the existing tree, commit-time refusal
  removing the partial file and the backend temp, overwrite delta success.
- HostConfig zero-value defaulting (1 GiB); config defaults snapshot,
  full-file parse, and negative-value validation for
  `plugin.system_storage_quota_mb`.
- `go test ./...` green, race clean, golangci-lint 0 issues.
