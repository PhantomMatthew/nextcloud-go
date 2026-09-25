# ADR-0094: Phase 5u atomic conditional DAV writes

- **Status**: Accepted
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: (none)

## Context

PUT evaluated `If-Match`/`If-None-Match` at the HTTP layer after a `Stat`,
then called `FS.Write` — a check-then-act window between the Stat and the
write where a concurrent PUT could rotate the etag and still be silently
overwritten (TOCTOU). The chunked-upload assemble path did the same with the
`If:` header's etag state list (`parseIfETag` after a `Stat`). Worse, plain
PUT never evaluated the `If:` etag state list at all, even though the
Nextcloud desktop client sends `If: (["etag"])` on PUT exactly for
lost-update protection; only lock tokens were consumed (via `CheckLock`).

## Decision

- `webdav.WriteCond` (`internal/webdav/cond.go`) carries the three
  conditional inputs: `IfETags` (etags from the `If:` state lists),
  `IfMatch`, and `IfNoneMatch` (both comma-split, quotes and `W/` stripped,
  `*` kept as an entry). `Evaluate(exists, etag)` maps the six rules to
  `ErrPrecondition`; a nil receiver is a zero-cost pass.
- `webdav.CondWriteFS` is an optional interface
  (`WriteIf(ctx, user, path, r, mtime, cond)`); the handler discovers it with
  the same assertion pattern as `ShareFS`/`LockFS`. Filesystems without it
  keep the legacy HTTP-layer Stat+Evaluate+Write sequence.
- `If:` header parsing (`ParseIfETags`) extracts the quoted contents of every
  `[...]` span inside parenthesized state lists: uri-tagged lists, multiple
  lists, and weak tags (`W/` sits outside the quotes and is stripped
  implicitly). Parenthesized lists whose first token (case-insensitive) is
  `Not` are skipped — NC clients never send them. Lock tokens (`<...>`) live
  outside `[...]` and are naturally excluded. Malformed input yields whatever
  was extracted; it never errors.
- `files.DAV` gains a 256-stripe FNV-1a lock table; `Write` and `WriteIf`
  share `writeConditional`, which resolves the target (own tree, incoming
  share with the PermUpdate/PermCreate check, or OCM-remote passthrough)
  outside the lock and runs the write core under the stripe for
  `user\x00path`. Preconditions evaluate against the in-lock filecache
  state, and event emission (`files.uploaded`) stays outside the lock.
  Inside the lock only Versions.Snapshot, Storage, and Meta calls happen —
  none re-enter write locking.
- `SQLStore.UpdateMetaIfETag` is the DB-level CAS guard:
  `UPDATE ... WHERE id = ? AND etag = ?`, with a zero-row result reported as
  the new `files.ErrETagConflict`, which `mapMeta` translates to
  `webdav.ErrPrecondition`. Unconditional writes keep the plain `UpdateMeta`.
  Version restore paths (`RestoreVersion`, `RollbackLatest`) route through
  `writeConditional` with a nil cond so they serialize per path too.
- `Uploads.Assemble` drops its local `parseIfETag` evaluation and calls
  `WriteIf` with `WriteCond{IfETags: ParseIfETags(ifHeader)}`; the Stat and
  overwrite checks stay so the 409 `ErrExists` semantics are preserved.
  `PublicDAV.WriteIf` mirrors `PublicDAV.Write` (resolve, jail, permission
  check, delegate).
- PUT builds the cond from all three headers; when all lists are empty (an
  `If:` header carrying only lock tokens included) the cond is nil and the
  always-on pre-write Stat is skipped — write errors surface identically
  from `Write` itself.

## Consequences

- Exactly-one-writer semantics per path in-process: N concurrent conditional
  writes with the same stale etag produce exactly one success and N−1 412s.
- Plain writes (`Write`, plugin and CLI callers, version restores) also
  serialize per path — they race the same storage keys.
- The zero-cond fast path drops one Stat per plain PUT.
- OCM-remote mount writes pass conditions through unenforced: the remote
  server owns the state and there is no filecache row to guard.
- Read/write skew during streaming (a reader seeing the new filecache row
  while old bytes are still in flight) is pre-existing and out of scope.
- Multi-process deployments can still clobber content between the in-lock
  evaluation and the storage write (the CAS guard covers only the filecache
  row); closing that needs staging-key publish semantics — listed as a
  follow-up.

## Verification

- `webdav` cond tests: `ParseIfETags` table (plain, uri-tagged, multiple
  lists, `Not` skipped, weak tags, lock-token-only, garbage) and `Evaluate`
  table covering all six rules hit/miss plus the nil receiver.
- `files` condwrite tests: matching/mismatching/missing If-etag, `If-Match`
  `*` and list, `If-None-Match` `*` (create-only) and list, and an
  8-goroutine race on one path with one stale etag asserting exactly one
  winner and the winner's bytes on disk — green under `-race`.
- `SQLStore.UpdateMetaIfETag` unit test: stale guard → `ErrETagConflict`,
  matching guard → update, replay → `ErrETagConflict`.
- Assemble tests: `If: (["etag"])` mismatch → 412 with the destination
  untouched, uri-tagged match → success.
- Handler tests: PUT against a `CondWriteFS` fake asserts the parsed cond
  arrives intact and a fake 412 maps; lock-token-only `If:` stays on the
  plain-Write fast path; `InMemoryFS` (no `WriteIf`) If-Match/If-None-Match
  tests exercise the legacy branch.
- Full suite + `go test -race` green; `golangci-lint run ./...` 0 issues;
  `go mod tidy` no diff (no new dependency).
