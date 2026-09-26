# ADR-0097: Phase 5w-1 SSE per-user keys — v3 envelope, KeyResolver, and owner-only wrapping

- **Status**: Accepted
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0096 (delivers its phase 1: envelope + resolver +
  owner-only wraps + eager-migration sweep)

## Context

ADR-0096 designed the per-user key hierarchy and phased its implementation.
This ADR records phase 1 **as built**: the v3 envelope format, the
`KeyResolver` seam and its SQL implementation, the schema, the filecache
integration, the opt-in config, and the fourth sweep direction. Phase 1
scope per the design: v3 header, KeyResolver, schema, config flag,
owner-only wraps on write, reads/Resolve, and `SweepRekeyV3` for eager
migration (lazy re-seal on write otherwise). Sharing integration (phase 2),
~~lifecycle/tooling (phase 3)~~ (**resolved by ADR-0099**), and
password-wrapped UKs (phase 4) remain
future increments; the threat model is unchanged from ADR-0096's statement
(phases 1–3 keep every UK sealed under the server-held master key).

Verified facts that shaped the build (beyond the design's list):

- The storage tree contains **non-DAV key layouts**: chunked-upload parts
  live at `uploads/<uid>/<tid>/...`, version snapshots at
  `versions/<uid>/<id>`, trash at `trash/<uid>/<loc>` (only renames, never
  `Create`), and the preview cache at `appdata_<instance>/previews`. A
  resolver that parses "uid = first path segment" would fail every version
  snapshot and chunk-part write — with versions wired in production, that
  breaks *all overwrites* in per-user mode.
- The DAV write path learns the key UUID only after the storage write
  closes; the decorator sits below the files layer and cannot call back
  into it. A tiny optional interface on the returned writer is the whole
  reporting channel.
- `Stat`/`List` must stay DB-free: the sweep's dry-run and every PROPFIND
  call `plainInfo` per file, and resolving keys there would put a database
  lookup on the directory-listing hot path.

## Decision

### 1. Encodings (pinned byte-exactly)

**v3 header** — `"NCGOENC3" (8B) || keyUUID (16B) || salt (32B)` = 56
bytes; `maxHeader` becomes 56. The chunk layer is reused verbatim: the data
key is `HMAC-SHA256(FK, salt)`, chunk framing, nonces, tags, and size math
are unchanged with header size 56. The keyUUID is 16 random bytes naming
the FK's wrap rows.

**User key (UK)** — 32 random bytes per user, minted lazily at the user's
first sealed write. Stored sealed:
`nonce(12) || AES-256-GCM(ring[keyID], UK, ad)` with
`ad = "NCGOUK1" || bigendian-uint64(user_id)`, where `keyID` is the ring
position of the current key at creation time. Unseal reads the row, picks
`ring[key_id]`, and GCM-opens with the same AD.

**File key (FK)** — 32 random bytes per file; wrapped:
`nonce(12) || AES-256-GCM(UK, FK, ad)` with
`ad = "NCGOFK1" || keyUUID(16B raw) || bigendian-uint64(user_id)`. A wrap
replayed onto a different file UUID or user fails authentication (pinned by
unit test).

New sentinel `ErrUnresolvableKey` for every resolver failure (unknown
keyUUID, missing rows, UK/FK unwrap failure): an operator key/config
problem that must never surface as `ErrIntegrity`, extending the ADR-0074
`ErrUnknownKeyID` precedent. No key zeroization (matches package posture);
stdlib crypto only — **zero new dependencies**.

### 2. The resolver seam

```go
type KeyResolver interface {
    Allocate(ctx context.Context, storageKey string) (keyUUID [16]byte, fk []byte, err error)
    Resolve(ctx context.Context, keyUUID [16]byte) (fk []byte, err error)
}
```

`FS` gains an optional `resolver`; `NewWithResolver(current, previous,
inner, res)` is the new constructor and `New`/`NewWithPrevious` delegate
with a nil resolver. **Nil resolver ⇒ v1/v2 behavior bit-identical
everywhere** — the ADR-0074 rollback carve-out extends verbatim: a
deployment that never enables the mode rolls back safely; once v3 files
exist, rollback strands them (same rule as v2).

- `Create` with a resolver: `Allocate` supplies `(keyUUID, FK)` (on error
  the inner writer is closed and the error returned), the header is
  `magicV3 || keyUUID || salt(rand)`, and the chunk writer runs on
  `dataKey(FK, salt)` exactly as today. The returned writer exposes the
  UUID through the neutral storage package's optional interface
  `storage.KeyUUIDWriter{ SealedKeyUUID() ([16]byte, bool) }` — v3 writers
  return `(uuid, true)`; v1/v2 writers simply do not implement it
  (documented on the interface).
- `Open` on a v3 header: parse `keyUUID = head[8:24]`, `salt = head[24:56]`,
  `Resolve` for the FK, then the unchanged chunk reader at header size 56.
  A v3 file on a nil-resolver FS fails with `ErrUnresolvableKey` naming the
  path and UUID — never a panic, never `ErrIntegrity`.
- `plainInfo` (Stat/List) recognizes v3 for the size math only — no
  `Resolve` (DB-free), and the ring-key existence check applies to v1/v2
  only. `headerLayout` is now a small layout struct (version + keyID +
  size); v1/v2 semantics are byte-identical.

### 3. SQLResolver and the owner derivation

`NewSQLResolver(db, keys)` copies the keyring (previous first, current
last — positions are the key IDs UK rows reference).

`Allocate`:

1. Parse the owner uid from the storage key: DAV keys are `uid/path`
   (first segment); the `uploads/`, `versions/`, and `trash/` namespaces
   nest the uid one level down, so there the second segment is the owner.
2. `SELECT id FROM users WHERE uid = ?` — unknown uid is an error; the
   resolver never invents users.
3. Load the UK (`SELECT sealed_uk, key_id FROM user_keys WHERE user_id`);
   on a miss, mint it, seal under the current ring key
   (`key_id = len(keys)-1`), and `INSERT`; on unique violation (a
   concurrent first write won) re-`SELECT` and unseal the winner's row —
   both racers return the same UK.
4. Fresh FK + keyUUID (crypto/rand), wrap with the pinned AD,
   `INSERT INTO file_keys`.

`Resolve`:

1. **Owner first**: `SELECT user_id FROM files WHERE key_uuid = ? LIMIT 1`.
2. **Fallback** (trash/versions/upload parts — the owner's filecache row
   is gone or never existed): `SELECT user_id FROM file_keys
   WHERE key_uuid = ? ORDER BY user_id LIMIT 1` — phase 1 writes only
   owner rows, so any row names the owner.
3. No rows → `ErrUnresolvableKey` wrapping the hex UUID.
4. Load that user's wrap row + UK, unwrap with the pinned AD. A GCM
   failure here is `ErrUnresolvableKey`, never `ErrIntegrity`.

**Why parse the uid from the storage key instead of a filecache lookup:**
at `Create` time the filecache row does not exist yet — the write is what
creates it — so there is nothing to look up. The storage key is the only
owner signal the decorator has, and every user-owned key layout carries the
uid. System trees (`appdata_<instance>/...`) have no owner: `Allocate`
rejects them, so phase 1 does not seal them under per-user keys — with
per-user mode on, writes to such trees fail loudly at `Create` (the known
interaction: the preview cache writes there, so previews and per-user keys
are not yet composable; a follow-up phase decides between an instance key,
a plaintext carve-out, or disabling previews).

### 4. Schema (migration 0020; sqlite + postgres + mysql kept in lockstep)

```sql
CREATE TABLE user_keys (
  user_id BIGINT PRIMARY KEY,
  sealed_uk BLOB NOT NULL,
  key_id INTEGER NOT NULL,
  created_ms BIGINT NOT NULL
);
CREATE TABLE file_keys (
  key_uuid BLOB NOT NULL,        -- 16 bytes
  user_id BIGINT NOT NULL,
  wrapped_fk BLOB NOT NULL,
  created_ms BIGINT NOT NULL,
  PRIMARY KEY (key_uuid, user_id)
);
ALTER TABLE files ADD COLUMN key_uuid BLOB NULL;
CREATE INDEX files_key_uuid_idx ON files(key_uuid);
```

Types follow each dialect's idiom (sqlite `INTEGER`/`BLOB`, postgres
`BIGINT`/`BYTEA`, mysql `BIGINT`/`VARBINARY(16)` for the indexed UUID
column). No FK clauses: lifecycle cleanup is ~~phase 3's explicit job~~
(**resolved by ADR-0099**) an explicit tooling job, not a cascade side
effect.

### 5. Filecache integration

`files.File` gains `KeyUUID []byte` (nil unless v3); `Insert`,
`UpdateMeta`/`UpdateMetaIfETag` (shared UPDATE builder), and `scanFile`
carry the column. `dav.write` asserts `storage.KeyUUIDWriter` on the closed
writer and sets the UUID on the inserted or updated row — including the
clearing case: rewriting a v3 file through a nil-resolver FS stores NULL,
matching the file's return to v1/v2. Move renames the object and keeps the
row (UUID follows); Copy Creates a fresh object (fresh UUID); trash and
version rows need no column (the resolver fallback covers them).

### 6. Config and wiring

`encryption.per_user_keys` (bool, default false); validation rejects it
without `encryption.enabled` (the dead-key rule). `app.openStorage` and the
CLI's `openStorage` both gained the DB handle: with the mode on they build
`NewSQLResolver(db, ring)` over the full ring (previous..., current last)
and `NewWithResolver`. The CLI's sweep commands build the resolver for
**every** subcommand in per-user mode: `encrypt-all` then seals plaintext
straight to v3, and `decrypt-all`/`rotate-keys` can read v3 files.

### 7. Sweep direction four and rotation notes

`SweepRekeyV3` (next iota, `"rekey-v3"`): target encoding "sealed v3".
Per file: plaintext → skip (compose with `encrypt-all`, the SweepRotate
rationale — in per-user mode that lands on v3 for free); v3 → skip;
v1/v2 → read through the FS, rewrite through `Create`. Running it without
a resolver on the FS is an error (mirroring SweepRotate's single-key
guard), and the CLI additionally guards on the mode being enabled, printing
the migration procedure otherwise. Abort-at-first-file on `ErrIntegrity` /
`ErrUnknownKeyID` / `ErrUnresolvableKey` extends the SweepRotate rule;
dry-run counts plaintext bytes via `enc.Stat`. CLI:
`ncgo-cli encryption rekey-v3 [--user uid] [--dry-run]` reporting
`rekey-v3: scanned=N rekeyed=M skipped=K failed=F bytes=B`.

`SweepRotate` now skips v3 files: their content keys are per-user FKs,
which master-key rotation never re-seals (ADR-0096). A v1/v2 file rotated
through a resolver-carrying FS lands in the v3 envelope — no longer
reachable with the retired key either, so the rotation goal holds in
per-user mode.

**Rotation and UK rows:** UK rows reference ring positions forever via the
append-only ring, so a master-key rotation needs no UK re-seal for
correctness — old-position keys stay readable by construction. ~~Phase 3
adds the hygiene re-seal (rewriting UK rows to the current position, a
small direct re-seal, not a sweep)~~ (**resolved by ADR-0099**: `rotate-keys`
re-seals UK rows) so retired keys can eventually leave the ring on the
far-future compaction path.

### Alternatives considered

- **Filecache lookup for the owner in Allocate** — impossible: the row is
  created by the write itself; at `Create` time it does not exist. The
  storage key carries the uid in every user-owned layout. Rejected.
- **First-segment-only uid parsing** — breaks `versions/<uid>/<id>` and
  `uploads/<uid>/...` writes, and with versions wired that fails every
  overwrite in per-user mode. The namespace-aware parse covers the repo's
  three non-DAV user key layouts. Rejected.
- **Silently skipping v3 sealing for ownerless system trees** (fall back
  to v1/v2 ring sealing for `appdata_*`) — hides a mode violation behind an
  implicit format choice and would strand those files outside the per-user
  migration story. Failing loudly at `Create` keeps the invariant "per-user
  mode ⇒ every new sealed user file is v3" total and visible. Rejected.
- **Zeroizing UK/FK material after use** — the package's existing posture
  holds no zeroization (the ring itself lives in process memory); adding it
  only for the new keys would be security theater. Deferred with the whole
  topic, not per-key.
- **`files.key_uuid` NOT NULL with a sentinel for v1/v2** — a nullable
  column is the honest representation (NULL = not a v3 file) and keeps the
  migration a cheap `ADD COLUMN` on existing deployments. Rejected.

## Consequences

- New config surface: `encryption.per_user_keys` (default false; validated
  against `enabled`). New tables `user_keys`, `file_keys`; new nullable
  `files.key_uuid` (+ index). Migration 0020 in all three dialect dirs.
- Mixed v1/v2/v3 trees are the normal migrated state; reads auto-detect per
  file, exactly as ADR-0052/0074 established. Lazy migration re-seals on
  next write; `rekey-v3` migrates eagerly.
- v3 files grow the header to 56 bytes (from 40/41); chunk framing, tags,
  and size math are otherwise unchanged, and `Stat`/`List` report plaintext
  sizes for all three versions without touching the database.
- Enabling the mode changes the write path only: `New`, `NewWithPrevious`,
  and every resolver-free read/write are bit-identical to the previous
  build (pinned by the unchanged ADR-0052/0074 test suites).
- `openStorage` gained a DB parameter in both `internal/app` and
  `cmd/ncgo-cli` (the only two keyring wiring sites, as in ADR-0074); a nil
  DB is valid exactly when the mode is off.
- Overwrites in per-user mode mint a fresh FK + UUID per write; the
  superseded wrap rows persist (versions/trash of the old object still
  resolve through them) and are ~~phase-3 lifecycle cleanup~~ (**resolved by
  ADR-0099**).
- Known limitation: system trees (`appdata_<instance>`, e.g. the preview
  cache) are not v3-eligible — writes there fail while the mode is on, so
  previews and per-user keys are not yet composable. Operators enable one
  or the other until a follow-up phase resolves the ownerless-tree story.
- Once the first v3 file exists, rolling back to a pre-v3 binary strands it
  (reads fail `ErrUnresolvableKey`) — documented in the config comment, the
  command help, and here, same rule as v2.

## Verification

- `internal/storage/encrypt/v3_test.go`: v3 round trips across chunk
  boundaries (0, 1, 100, 64KiB-1, 64KiB, 64KiB+1, 3*64KiB+17 — empty file
  stores the 56-byte header only); header sniff for all three magics;
  v3 `Stat`/`List` plaintext sizes; unknown keyUUID → `ErrUnresolvableKey`
  naming the UUID, never `ErrIntegrity`; tampered v3 chunk →
  `ErrIntegrity`; v3 file on a nil-resolver FS → `ErrUnresolvableKey` (no
  panic) while `Stat` stays correct and DB-free; mixed v1/v2/v3/plaintext
  tree reads and sizes per file.
- `internal/storage/encrypt/resolver_sql_test.go`: constructor validation
  and ring-copy immutability; Allocate/Resolve over a real DB with the UK
  row proven to open under the current ring position with the pinned AD and
  the FK wrap under the pinned AD; AD binding (wrong user_id or keyUUID in
  the AD fails); concurrent first-write Allocates → one `user_keys` row,
  all succeed (unique-violation re-select); unknown user/UUID errors;
  owner-first then fallback resolution after the filecache row is deleted;
  ring position recorded on the UK row with a two-key ring. `SweepRekeyV3`:
  mixed v1/v2/v3/plaintext tree → v1/v2 become v3 (content intact,
  `file_keys` rows present), plaintext and v3 skipped, idempotent re-run,
  dry-run zero writes, guard error without a resolver; `SweepRotate` skips
  v3 and lands v1 on v3 through a resolver FS.
- `internal/files/dav_keyuuid_test.go`: per-user DAV over a real DB (trash
  + versions wired as production) — Write sets the row's key_uuid and
  reads back; overwrite (version snapshot through the namespaced
  `versions/<uid>/<id>` key included) mints a fresh UUID; Move keeps the
  UUID; Copy mints a distinct one; a plain FS keeps key_uuid nil.
- `cmd/ncgo-cli/encryption_test.go`: end-to-end rekey-v3 on localfs (v1
  files → guard error with the mode off → enable → dry-run → real run with
  exact report lines → v3 magics → content intact via a resolver-carrying
  storage → wrap-row and UK counts → idempotent re-run → `encrypt-all`
  seals plaintext straight to v3); `internal/config`: default/override
  parsing, valid-with-enabled, and the per_user_keys-without-enabled
  validation case; `internal/migrations`: up/down/up now covers 0020
  (tables, column, index lifecycle).
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` no-op (zero new
  external dependencies — stdlib crypto only), `go test -race ./...` green.
