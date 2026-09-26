# ADR-0099: Phase 5w-3 SSE per-user keys — lifecycle + tooling

- **Status**: Accepted
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0096 (delivers its phase 3: lifecycle + tooling),
  ADR-0097 (resolves the "phase 3's job" deferrals: lifecycle cleanup,
  rotation UK re-seal, superseded-row cleanup), ADR-0098 (resolves the
  phase-3 repair/cleanup deferrals: stale rows of deleted users, skew
  repair, import/bootstrap wrapping)

## Context

ADR-0096 phased the per-user key hierarchy; ADR-0097 built phase 1 (v3
envelope, `KeyResolver`, owner-only wraps) and ADR-0098 phase 2 (share
wrap/revoke). Both deferred the same cluster of lifecycle concerns to
phase 3, which this ADR records **as built**: eager user-key minting at
account creation, key-row cleanup at account deletion, UK re-sealing after
master-key rotation, and the operator tooling that makes key-hierarchy
health visible and repairable. The threat model is unchanged from
ADR-0096's statement (phases 1–3 keep every UK sealed under the
server-held master key; revocation is policy-level until phase 4).

Verified facts that shaped the build:

- **The bootstrap admin is created before the storage layer exists.**
  `app.New` calls `users.EnsureBootstrapAdmin` ~40 lines before
  `openStorage` returns the key resolver, so an eager mint at creation
  cannot cover the very first account. Lazy minting inside
  `loadOrCreateUK` (phase 1) already heals this on the admin's first
  sealed write; `encryption reconcile` backfills it explicitly.
- **`users.SQLStore.Delete` resolves the uid before deleting**
  (`GetByUID` → memberships → row). A deletion hook addressed by uid must
  fire while the uid still resolves — after the row delete there is no
  `users.id` left to name the doomed key rows.
- **The phase-2 hook precedent is structural.** `users` imports neither
  `files` nor `encrypt`; `MemberKeysHook` is satisfied structurally by
  `*files.KeySharer`. The user-lifecycle hook follows the same shape:
  an interface in `users`, satisfied structurally by
  `*encrypt.SQLResolver`.
- **Wrap rows are advisory until phase 4** (ADR-0098 §5): server-side
  reads resolve owner-first through `files.key_uuid` and never consult
  recipient rows. Re-wrapping an already-wrapped (key, user) pair is a
  no-op, so a repair pass over every existing share is safe and
  idempotent by construction.
- **No SQL-side string concatenation exists anywhere in the repo**, and
  the inventory/repair queries need none: `?` placeholders, `NOT IN`, and
  `NOT EXISTS` are dialect-agnostic across sqlite/postgres/mysql, so this
  increment needs **no schema or migration changes**.

## Decision

### 1. The UserKeys lifecycle hook (`internal/users/sqlstore.go`)

`users.SQLStore` gains an optional `UserKeys UserKeysHook` field mirroring
the ADR-0098 `MemberKeys` pattern exactly:

```go
type UserKeysHook interface {
    OnUserCreated(ctx context.Context, uid string) error
    OnUserDeleted(ctx context.Context, uid string) error
}
```

`*encrypt.SQLResolver` satisfies it structurally — `users` must not import
`encrypt` — and nil disables the hook. `Create` fires `OnUserCreated`
after the post-insert re-read; `Delete` fires `OnUserDeleted` **between
the membership cleanup and the row delete** — the point where the
operation can no longer fail on memberships but the uid still resolves to
a `users.id` (see Context; firing after the row delete would leave the
hook with no addressable rows). Invocation is best-effort: a hook failure
is Warn-logged via the existing `Logger` field and never fails
Create/Delete. Wired in `app.New` (`userStore.UserKeys = keyResolver`), in
`ncgo-cli user add/delete`, and in `ncgo-cli import-nextcloud users`
(imported accounts get UKs minted exactly like local ones), each gated on
`encryption.enabled && encryption.per_user_keys`.

**Bootstrap-admin gap (accepted):** the bootstrap admin is created before
the resolver exists (Context), so its UK is minted lazily on its first
sealed write or by `encryption reconcile`. Lazy minting is the phase-1
safety net that makes this gap cosmetic; it is documented rather than
restructured because moving admin creation after storage would reorder
`app.New`'s migration/instance-id/storage bootstrap for zero behavioral
gain.

### 2. Resolver lifecycle methods (`internal/storage/encrypt/resolver_lifecycle.go`)

All methods are concrete-only on `*SQLResolver`; the narrow `KeyResolver`
interface the storage decorator consumes is untouched.

- `OnUserCreated(ctx, uid)` — uid→`users.id` (unknown uid is an error: the
  hook fires from a successful Create, so a missing user is a real
  inconsistency), then the phase-1 `loadOrCreateUK`. Idempotent.
- `OnUserDeleted(ctx, uid)` — unknown uid → nil (no-op, mirrors
  `UnwrapKeyFor`; makes hook replays safe). Else
  `DELETE FROM file_keys WHERE user_id = ?` then
  `DELETE FROM user_keys WHERE user_id = ?` — the user's own rows only.
  Wrap rows of OTHER users and `files.key_uuid` are untouched: sealed
  files a deleted user still owned become unresolvable, the same operator
  territory as `users.SQLStore.Delete`'s "files are not cascaded" warning
  (and the `ncgo-cli user delete` help now says so explicitly, with the
  UNREADABLE consequence spelled out).
- `ResealUserKeys(ctx) (int, error)` — the ADR-0097 rotation hygiene:
  every `user_keys` row not sealed under the current ring position
  (`key_id = len(keys)-1`) is unsealed under its recorded position and
  re-sealed under the current key with the same pinned AD. A row whose
  key_id the ring no longer holds, or that fails authentication, aborts
  the pass with `ErrUnresolvableKey` naming the user — matching `loadUK`.
  Idempotent: a re-run finds every row at the current position.
- `PruneStaleKeys(ctx) (userKeys, fileKeys int, err error)` — the repair
  half of `OnUserDeleted` for deletions that ran without the hook (raw
  SQL, imports, crashes): `DELETE … WHERE user_id NOT IN (SELECT id FROM
  users)` on both tables, reporting `RowsAffected` per table.
- `MintMissingUserKeys(ctx) (int, error)` — backfill: `loadOrCreateUK`
  for every user with no `user_keys` row (accounts predating per-user
  keys or the hook — including the bootstrap admin). Idempotent.
- `KeyInventory` + `Inventory(ctx)` — the health read model: users total /
  with UK, UKs sealed under retired key ids, stale UKs, wrap rows and
  distinct key UUIDs, stale wraps, v3 files, and **broken v3 files** — a
  filecache row whose `key_uuid` has no wrap row for its owner, which can
  never resolve and is UNREADABLE. Nine plain `COUNT(*)` queries, all
  dialect-agnostic; no schema beyond migration 0020.

### 3. `encryption status` per-user section

With per-user keys on, `status` appends the inventory after the existing
key-file report: users total/with-key, retired-key-id UKs (hint:
rotate-keys re-seals), wrap rows across key UUIDs, v3-sealed files, stale
rows (hint: reconcile prunes), and broken v3 files. `BrokenV3Files > 0`
fails the command non-zero after printing — the same fail-closed posture
`status` already has for missing/unusable key files.

### 4. `rotate-keys` re-seals user keys

After a successful non-dry-run rotation sweep in per-user mode, the CLI
runs `ResealUserKeys` and prints `re-sealed N user key(s) under key id K`
(K = the current ring position). A re-seal error fails the command. UK
rows were already *correct* under the append-only ring (ADR-0097); this
is the hygiene step that lets retired master keys eventually leave the
ring on the far-future compaction path.

### 5. `encryption reconcile [--dry-run]` — the repair tool

Requires `encryption.enabled && encryption.per_user_keys` (the guard
error prints the migration procedure, rekey-v3 style). Three idempotent
steps over the live database:

1. **Mint** missing UKs (`MintMissingUserKeys`).
2. **Wrap share recipients**: every user/group share of every owner
   (`users.List` → `ListByOwner`) goes through
   `files.KeySharer.WrapForShare` — grant hooks only wrap shares created
   after the mode was enabled, so this backfills pre-existing shares and
   hook-missed skew; link/OCM shares are skipped inside `WrapForShare`;
   per-share errors are collected and reported, not fatal to the pass.
3. **Prune** stale rows (`PruneStaleKeys`).

Summary: `minted N user key(s)` / `processed S share(s) (E error(s))` /
`pruned U user key(s), W wrap(s)`; non-zero exit when wrap errors
occurred. `--dry-run` performs no writes: it reports the inventory-derived
counts (users without a UK as `UsersTotal − UsersWithUK`, stale UKs and
wraps) and the number of shares that would be processed, marked `dry-run`.

### 6. Why hooks stay best-effort

Recipient wraps — and lifecycle key rows generally — are the phase-4
substrate plus an audit record, never a read-path dependency (ADR-0098
§5: reads resolve owner-first through `files.key_uuid`). A key-row outage
must therefore never fail account creation or deletion. Every skew a
dropped hook can create (missing UK, stale rows, unwrapped share
recipients) is visible in `encryption status` and repairable by
`encryption reconcile`, and all three repair steps are idempotent — the
system converges by re-running a command, not by making runtime
operations fragile.

### 7. Why still no FK constraints

Migration 0020 deliberately omitted FK clauses ("lifecycle cleanup is an
explicit job, not a cascade side effect"). This phase keeps that: the
purge order (`file_keys` before `user_keys` on deletion; `NOT IN` prunes)
is explicit, unit-tested code rather than schema-side behavior, and it
avoids dialect divergence — sqlite FK enforcement is a per-connection
pragma, so a cascade would behave differently depending on which pool
connection executes a delete. Explicit purges plus inventory visibility
give operators a checkable story instead of a hidden one.

## Alternatives considered

- **`ON DELETE CASCADE` from `users` into the key tables** — rejected per
  §7: connection-dependent on sqlite, hidden from tests, and it would not
  cover rows orphaned by raw deletes on other databases anyway.
- **Firing `OnUserDeleted` after the row delete** — impossible under a
  uid-addressed hook: once the users row is gone the uid no longer
  resolves to the `users.id` the key tables are keyed by, and the pinned
  "unknown uid → no-op" guard would make the hook a permanent no-op. The
  hook fires between membership cleanup and the row delete instead
  (§1); a row-delete failure afterwards leaves a live user without key
  rows, which lazy minting heals on next write.
- **Resealing UKs inside the sweep's file walk** — UK rows are database
  state, not storage files; the sweep walk would need a side channel for
  a job that is one indexed table pass. A direct re-seal after the sweep
  keeps each mechanism over its own store. Rejected.
- **Aborting reconcile at the first wrap error** — one bad share (e.g. a
  target deleted between listing and wrap) would block the repair of
  every later share; collecting errors and exiting non-zero matches the
  sweep's OnError posture. Rejected.
- **Widening `KeyResolver` with the lifecycle methods** — the decorator
  needs only `Allocate`/`Resolve`; lifecycle and repair are resolver-
  specific surface, kept concrete per the ADR-0098 concrete-only
  precedent. Rejected.

## Consequences

- `users.SQLStore` gains `UserKeys`; the `UserKeysHook` interface joins
  `MemberKeysHook`; `Create`/`Delete` fire the hooks (Delete between
  membership cleanup and the row delete — documented on the method);
  `Logger` now covers both hooks. No interface (`users.Store`) changes.
- `encrypt.SQLResolver` gains `OnUserCreated`/`OnUserDeleted`/
  `ResealUserKeys`/`PruneStaleKeys`/`MintMissingUserKeys`/`Inventory` and
  the `KeyInventory` struct — concrete-only; `KeyResolver` untouched; no
  schema or migration changes.
- `app.New` wires `userStore.UserKeys = keyResolver` in per-user mode;
  the bootstrap admin's UK stays lazy (documented gap, §1).
- `ncgo-cli`: `user add`/`user delete` and `import-nextcloud users` wire
  the hook when per-user keys are on (delete's help warns about
  UNREADABLE sealed files); `encryption status` prints the inventory and
  fails non-zero on broken v3 files; `rotate-keys` re-seals UKs after a
  successful rotation; new `encryption reconcile [--dry-run]`; the root
  help's per-user migration procedure gains step 4 (reconcile) and the
  rotate-keys re-seal note.
- Skew from pre-existing users/shares, hook-less deletions, or crashed
  hooks is now visible (`status`) and convergent (`reconcile`), without
  making any runtime operation depend on key-row writes succeeding.
- Zero new dependencies (stdlib only).

## Verification

- `internal/storage/encrypt/resolver_lifecycle_test.go`:
  `OnUserCreated` mints (row opens under the current ring key with the
  pinned AD), is idempotent (same bytes, one row), and errors on an
  unknown uid; `OnUserDeleted` purges the user's UK + wrap rows, leaves
  other users' rows intact, and treats unknown uids and post-delete
  replays as no-ops; `ResealUserKeys` moves UKs from ring position 0 to 1
  with the UK bytes unchanged (unsealed both ways), is idempotent,
  returns 0 on zero rows, and aborts with `ErrUnresolvableKey` naming the
  user on an out-of-ring key id; `PruneStaleKeys`/`MintMissingUserKeys`
  backfill and prune exactly the stale/missing rows (idempotently);
  `Inventory` matches a hand-computed `KeyInventory` including one stale
  UK and one broken v3 file; `TestSQLResolverConcurrentOnUserCreated`
  (8 goroutines, one row) pins the insert-race re-select through the new
  entry point under `-race`.
- `internal/users`: `TestSQLStoreUserKeysHook` — the hook fires on
  Create/Delete with the right uid; a failing hook never fails the
  operation and is Warn-logged when `Logger` is set; nil hook leaves
  existing tests untouched.
- `cmd/ncgo-cli/encryption_test.go`: `status` prints the per-user section
  (temp file-backed sqlite + temp master key) and exits non-zero on a
  fabricated broken v3 file; `rotate-keys` prints
  `re-sealed 1 user key(s) under key id 1` (UK minted under a retired
  key, ring of 2) and `0` on re-run; `reconcile --dry-run` reports
  without writing; a real reconcile run end-to-end — users created
  WITHOUT hooks, a v3-sealed file, a user share, a stale deleted-user UK —
  mints the missing UK, creates the recipient's `file_keys` row (queried
  directly), prunes the stale row, and is idempotent on re-run; the
  per-user-keys-off guard prints the migration procedure; `user add`
  mints and `user delete` purges key rows with per-user config.
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` no-op (zero
  new dependencies), `go test -race ./...` green.
