# ADR-0098: Phase 5w-2 SSE per-user keys — share wrap/revoke

- **Status**: Accepted
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0096 (delivers its phase 2: share wrap/revoke), ADR-0097
  (revises the "superseded wrap rows persist" note — see §3)

## Context

ADR-0096 designed per-recipient key wrapping and phased it; ADR-0097 built
phase 1 (v3 envelope, `KeyResolver`, owner-only wraps). This ADR records
phase 2 **as built**: wrapping file keys for share recipients on grant,
unwrapping on revoke, group membership churn, write-path auto-wrapping for
shared folders, and overwrite continuity — with the ADR-0094 write-path
concurrency pinned by tests. The threat model is unchanged from ADR-0096's
statement (phases 1–3 keep every UK sealed under the server-held master
key; revocation is policy-level until phase 4).

Verified facts that shaped the build (beyond the phase-1 list):

- **Version snapshots re-seal under their own fresh key UUID.**
  `Versions.copyToVersion` reads the old object through the encrypt FS and
  writes it back through `Create`, which `Allocate`s a fresh FK + UUID with
  its own owner wrap row. No long-lived object references the superseded
  UUID after an overwrite: the old storage object is overwritten in place,
  trash renames only the current (live-key) object, and snapshots carry
  their own keys. Deleting all old-UUID wrap rows on overwrite therefore
  strands nothing addressable (pinned by test).
- **No SQL-side string concatenation exists anywhere in the repo.** Every
  prefix query builds its LIKE pattern in Go; the only set-membership idiom
  is the `IN (...)` placeholder list (`ListBySharee`).
- **Import direction:** `sharing` imports `files`; `users` imports neither.
  A `KeySharer` living in `files` can consume the share store and the users
  store only through narrow structural interfaces, and the users store's
  membership hook must be equally structural.
- **The expire sweep bulk-deletes by id** (`DeleteExpired` returns ids, not
  rows); unwrapping for reaped shares requires reading the doomed rows
  first.
- **Incoming-share writes are remapped to the owner** before the locked
  write core (`writeConditional`), so inside `dav.write` the resolved user's
  numeric id IS the owner's — the write-path hook needs no extra lookup.
- **The ADR-0094 stripe lock covers writes, not share deletes.** A write
  into a shared folder races an unshare of that folder with no common lock;
  the invariant must come from the wrap logic itself.

## Decision

### 1. Resolver wrap API (`internal/storage/encrypt/resolver_sql.go`)

Three methods join `Allocate`/`Resolve`, reusing the phase-1 lazy-UK and
FK-wrap internals (factored into `wrapFKForUser`):

- `WrapKeyFor(ctx, keyUUID, uid)` — uid→`users.id` (unknown uid is an
  error; the resolver never invents users), FK = `Resolve(keyUUID)`
  (unknown UUID → `ErrUnresolvableKey`), lazily create the recipient UK,
  insert the wrap with the pinned AD `"NCGOFK1"||keyUUID||be64(user_id)`.
  Unique violation → nil: **re-wrapping is idempotent**, so grant hooks
  retry freely.
- `UnwrapKeyFor(ctx, keyUUID, uid)` — delete the row; absent → nil. Two
  guards: the user owning the `files.key_uuid` row for keyUUID is **never
  unwrapped** (a revoke must never orphan a file from its owner — the owner
  can be a member of a group the file is shared to), and an unknown uid is
  a no-op (no live user means no addressable row; ~~stale rows of deleted
  users are phase-3 lifecycle cleanup~~ (**resolved by ADR-0099**: purged by
  `OnUserDeleted` / `PruneStaleKeys`)).
- `ReWrapSharees(ctx, oldUUID, newUUID)` — overwrite continuity: FK =
  `Resolve(newUUID)` (the owner row exists from `Allocate`); every
  `user_id` in `file_keys(oldUUID)` that is not the new key's owner gets a
  wrap of the new FK (idempotent insert); then **all** old-UUID rows are
  deleted. A recipient wrap failure aborts *before* the delete so a retry
  continues where it stopped; a re-run after success finds no old rows and
  re-inserts nothing (idempotent overall).

### 2. KeySharer (`internal/files/keyshares.go`) and the hook inventory

`KeySharer` is the single service keeping `file_keys` in step with shares,
constructed once in `app.New` when `encryption.per_user_keys` is on and
injected into every hook (nil everywhere when off — zero overhead). Its
dependencies are narrow seams: `KeyWrapper` (satisfied by
`*encrypt.SQLResolver`), `KeyShareMeta` (`*files.SQLStore`),
`ShareKeyLookup` (`*sharing.SQLShareStore`, satisfied structurally — files
cannot import sharing), `GroupMemberLookup` (`users.Store`). Every method
returns errors **for the caller to log** and is best-effort by contract
(§5). Files without a v3 key UUID (plaintext, v1/v2 — NULL `key_uuid`) are
skipped everywhere.

| Event | Hook | Effect |
|---|---|---|
| Share created (user/group) | `sharing.Service.Create` → `Keys.WrapForShare` | wrap the target file's key (user) or every member (group); folder shares walk the sealed subtree (`ListSealedSubtree`), continuing past per-file errors |
| Share deleted | `sharing.Service.Delete` → `Keys.UnwrapForShare` | mirror unwrap |
| Lazy expiry (`GetForOwner`/`ListForOwner`/…) | `expireIfNeeded` → `UnwrapForShare` | same as delete |
| Background expiry | `sharing.NewExpireJob` (new `keys` param) | `ListExpired` first, reap, then unwrap exactly the reaped shares |
| Write into a covered path | `dav.write` → `WrapForWrite(ownerUserID, path, newUUID)` | wrap the fresh key for every recipient of every **covering** share |
| Overwrite with a previous v3 key | `dav.write` → `ReWrapForOverwrite(old, new)` | delegate to `ReWrapSharees` when old is non-zero and differs |
| Group member join/leave | `users.SQLStore.AddGroupMember`/`RemoveGroupMember` → `MemberKeys` hook | `OnGroupMemberAdded/Removed`: wrap/unwrap every share granted to that group (`ForGroup`) |

Write-path placement: inside the stripe-locked core, after the filecache
row (with the new key UUID) is persisted; failures are Warn-logged via the
DAV's new optional `Logger` and never fail the write. The membership hook
is an optional field on `users.SQLStore` (no interface change — the
`web/login_v2` stubs are untouched); failures are Warn-logged via an
optional `Logger` field. Wired in `app.New` and in `ncgo-cli group
adduser/removeuser` (the only runtime membership writers besides
bootstrap/import), where hook failures surface as stderr warnings.

### 3. Overwrite continuity rule (amends ADR-0097)

**Wrap rows belong to the live file.** An overwrite mints a fresh FK + UUID
(phase 1); continuity for sharees means following the current key, so
`ReWrapSharees` carries non-owner recipients to the new UUID and deletes
ALL old-UUID rows — including the old owner row. ADR-0097's "superseded
wrap rows persist … ~~phase-3 lifecycle cleanup~~" (**resolved by ADR-0099**)
is revised: they persist
only until the next overwrite's carry. Deletion is safe because nothing
addressable references the old UUID afterwards (see Context): version
snapshots taken during the overwrite were re-sealed under their own fresh
UUIDs (their wrap rows are untouched — pinned by test: the snapshot reads
back after the carry), the old storage object no longer exists, and trash
only ever holds objects sealed under a live key.

### 4. Covering-share query semantics

`Covering(ownerUserID, path)` returns the user/group shares of the owner
whose target **is the path or an ancestor of it** — exactly the shares
whose recipients gain access to whatever is written at `path`. The ancestor
set (the path, each parent, and `/`) is computed in Go and matched with an
`IN (...)` placeholder list — the repo's dialect-neutral idiom, avoiding a
`? LIKE path || '/%'` that would need per-dialect string concatenation (no
precedent exists). Link and OCM-remote shares are excluded by the type
filter: their recipients have no user key, so no wrap is possible. Acceptance
state is ignored: wrap rows are the phase-4 substrate plus an audit record,
never a read-path grant, so wrapping a not-yet-accepted share is harmless
and revocation deletes regardless.

### 5. Best-effort rationale

Server-side reads NEVER use recipient rows (owner-first/any-row `Resolve`
— ADR-0097); recipient rows are the phase-4 substrate and an audit record.
Therefore no user-visible operation — share, unshare, write, membership
change, expiry — may fail on a wrap error. Every hook is nil-checked,
Warn-logs failures (service `warn`, expire job, DAV `Logger`, users
`Logger`, CLI stderr), and continues. Skew that escapes the hooks (a crashed
hook, a lost race remnant) is ~~phase-3~~ repair/status tooling territory
(**resolved by ADR-0099**), not
a runtime failure mode.

### 6. Concurrency invariant (ADR-0096's phase-2 requirement)

A write into a shared folder racing the folder's unshare must leave no
error on either side, and the file's recipient wrap row may exist **iff**
the share still exists. The stripe lock does not cover share deletes, so
`WrapForWrite` closes the window itself: after wrapping the recipients of
the covering shares it found, it **re-reads the covering set** and unwraps
any wrapped recipient no longer covered. The last actor to touch a row then
always leaves it consistent with the share table, for any interleaving
(pinned under `-race`, both directions of the invariant). The re-read costs
one extra query per write and runs only when the write actually wrapped
something — the common no-share case short-circuits after the first
`Covering` call.

### 7. Explicit skips

- **Link and OCM-remote shares**: no user key exists for the recipient;
  debug-logged no-ops.
- **Import and bootstrap membership changes**: not wired — import ordering
  (shares may not exist yet) makes wrapping premature; ~~phase-3 repair
  reconciles~~ (**resolved by ADR-0099**: `ncgo-cli encryption reconcile`).
- **`appdata_*` system trees**: unchanged from phase 1 (no owner; writes
  there fail loudly in per-user mode).
- **Stale wrap rows of deleted users / crashed hooks**: ~~phase-3 lifecycle
  and repair tooling~~ (**resolved by ADR-0099**).

## Alternatives considered

- **Per-dialect `LIKE path || '/%'` for Covering** — needs a dialect switch
  (`||` vs `CONCAT`) with zero repo precedent; the Go-computed ancestor set
  + `IN` list is dialect-neutral and matches `ListBySharee`. Rejected.
- **`DeleteExpired` returning rows instead of additive `ListExpired`** —
  changing the `files.ShareStore` interface ripples into test stubs for no
  benefit; the expire job type-asserts the optional lister (the
  `KeyUUIDWriter` optional-interface precedent). Rejected.
- **Serializing share deletes against the write stripe lock** — share
  operations would block on unrelated writes and the lock table would need
  cross-package reach; advisory rows only need eventual consistency, which
  the reconciliation re-read provides. Rejected.
- **Failing share/write/membership operations on wrap errors** — recipient
  rows are not read-path dependencies until phase 4; a wrap outage must not
  take down sharing. Rejected (§5).
- **Keeping the old owner row for trash/versions** — the phase-1 analysis
  re-checked for phase 2 shows snapshots re-seal under fresh UUIDs and the
  old object is gone; a surviving row would be unreferenced garbage.
  Rejected (§3).

## Consequences

- New resolver API (`WrapKeyFor`/`UnwrapKeyFor`/`ReWrapSharees`); new
  `files.KeySharer` with narrow seams; new store methods
  `files.SQLStore.ListSealedSubtree`, `sharing.SQLShareStore.Covering` /
  `ForGroup` / `ListExpired` (all concrete-only — no interface changes).
- `sharing.Service` gains optional `Keys`; `NewExpireJob` gains a trailing
  `keys` parameter; `files.DAV` gains optional `KeySharer` + `Logger`;
  `users.SQLStore` gains optional `MemberKeys` + `Logger`. All nil-safe:
  with per-user mode off, behavior is byte-identical to phase 1.
- Per-user mode writes now do up to two covering-share queries (wrap +
  reconciliation) only when shares cover the path; unshared writes cost one.
- Overwrites in per-user mode delete the superseded key's wrap rows after
  carrying recipients (§3); version snapshots keep resolving (fresh UUIDs).
- `ncgo-cli group adduser/removeuser` load the keyring and wire the
  membership hook in per-user mode; import/bootstrap stay unwired
  (documented).
- Zero new dependencies (stdlib only).

## Verification

- `internal/storage/encrypt/resolver_wrap_test.go`: WrapKeyFor creates the
  recipient UK lazily (opens under ring position 0 with the pinned AD), the
  wrap carries the owner's FK and opens with the pinned AD, re-wrap is
  idempotent, unknown uid errors, unknown UUID → `ErrUnresolvableKey`;
  UnwrapKeyFor deletes + idempotent + **owner guard** (owner row survives)
  + unknown-uid no-op; ReWrapSharees carries non-owner recipients (rows
  open with the pinned AD under the new FK), deletes all old rows, skips
  the owner duplicate, idempotent re-run, same-UUID no-op.
- `internal/files/keyshares_test.go` (real DB, real resolver, per-user
  DAV with trash+versions wired as production): user-share file →
  recipient row, unshare → row gone + owner row intact, link share wraps
  nothing; folder share → subtree rows for v3 files only (NULL `key_uuid`
  skipped); write into a shared folder → auto-wrapped by the write-path
  hook; overwrite → recipients carried to the new UUID, old rows gone, the
  version snapshot re-sealed under its own UUID still reads back; group
  share → all members, member join → wrapped, member leave → unwrapped;
  expired share via the expire job → rows gone. **Concurrency pin**
  (`TestKeyShareWriteUnshareRace`, 16 iterations + the unrelated-unshare
  control): no error either side, recipient row exists iff the share
  exists, green under `-race`.
- `internal/files/keyshares_unit_test.go`: reconciliation re-read unwraps a
  mid-write-vanished share; no-share writes skip the re-read; link/remote
  no-ops; group wrap/unwrap fan-out; subtree skips unsealed files;
  membership + overwrite delegation; wrapper errors surface for the caller.
- `internal/sharing/keyhooks_test.go`: Create/Delete fire the hook
  (user/group only), lazy expiry unwraps, a failing hook never fails the
  operation; expire job unwraps exactly the reaped shares and tolerates
  hook failure.
- `internal/users`: membership hook fires on add/remove; failing hook never
  fails the change; nil hook leaves existing tests untouched.
- `internal/app`: `openStorage` with encryption on and per-user keys off
  returns a nil resolver and round-trips (pins the typed-nil interface
  widening hazard), and `New` in per-user mode wires one KeySharer into the
  DAV, the sharing service, and the users membership hook.
- Store tests: `ListSealedSubtree` (sealed-only, self, unsealed skip),
  `Covering` (exact/ancestor/root/type/owner filters, prefix-but-not-
  ancestor exclusion), `ForGroup`, `ListExpired` (boundary + no-expiry).
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` no-op (zero new
  dependencies), `go test -race ./...` green.
