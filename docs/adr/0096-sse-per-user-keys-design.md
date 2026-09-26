# ADR-0096: Phase 5w server-side encryption per-user keys — key hierarchy and wrapping design

- **Status**: Accepted (design; implementation phased as below)
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0074 (supplies the per-user-keys design it deferred)

## Context

Server-side encryption today (ADR-0052 + ADR-0074) is a **user-agnostic
storage decorator**: every file's data key is `HMAC-SHA256(masterKey,
salt)`, the decorator sits below the database and sees only storage keys
(`uid/path`), and the append-only master keyring gives rotation. ADR-0074
deferred per-user keys with two named gaps: *"sharing requires
per-recipient key wrapping and an admin-recovery design."* This ADR supplies
both, and phases the implementation.

Verified facts that shape the design:

- **Shared content is stored once.** Incoming-share writes are remapped to
  the owner's path (`writeMaybeIncoming`) and sealed under the owner's
  storage key — so "seal the same object per recipient" is impossible at
  the storage layer. Nextcloud's answer is a per-file random key wrapped
  per recipient; we adopt the same envelope shape.
- **Storage objects follow renames** (`Move` → `Storage.Rename`), so key
  material must be addressable independent of path: the header carries a
  key UUID, not a path-derived reference.
- **The decorator has no database handle.** A narrow `KeyResolver` seam is
  the entire dependency inversion needed.
- **App passwords authenticate without the login password**
  (`internal/auth` app-password verifier) — any design that wraps user keys
  under login passwords must say what token sessions do.
- The v2 header (`"NCGOENC2" || keyID || salt`, ADR-0074) already
  established the version-marker convention and the bit-compatibility
  carve-out this ADR reuses.

## Threat model, stated plainly

Phases 1–3 keep every user key sealed under the server-held master key.
**The at-rest threat model is unchanged**: an attacker with the master key
still unwraps everything, and share revocation is policy-level (the server
can unwrap via any remaining row). What phases 1–3 buy: per-user key
separation (one UK leak exposes one user's wraps, not the tree), a real
per-recipient wrapping infrastructure, the admin-recovery story, and the
migration path. **Only phase 4 (password-wrapped user keys) changes the
threat model** — a server-at-rest compromise then exposes no file content
for users without an active unlocked session. Master-key mode remains the
recommended default for most deployments (as upstream Nextcloud also
recommends), because phase 4 trades away token-only client sessions.

## Decision

### 1. Key hierarchy

- **File key (FK)**: random 32 bytes per file, generated on first sealed
  write. Replaces HMAC derivation for v3 files. Content sealing is
  unchanged: AES-256-GCM, 64 KiB chunks, salt-derived nonces (ADR-0052).
- **User key (UK)**: random 32 bytes per user, generated lazily at the
  user's first sealed write (or user creation once phase 3 lands). The UK
  row is itself sealed under the current master key with a v2-style key-ID
  byte, so master-key rotation re-seals UK rows (small table — a direct
  re-seal inside `rotate-keys`, not a sweep). File keys never change on
  master rotation: only the UK envelope does.
- **Wrap**: `AES-256-GCM(UK, FK)` with associated data
  `keyUUID(16) || userID` — a wrap replayed onto a different file or user
  fails authentication.

### 2. v3 header and the resolver seam

`"NCGOENC3" (8B) || keyUUID (16B) || salt (32B)` = 56 bytes. The keyUUID
names the FK set; the salt keys the chunk construction exactly as before.
The decorator gains one optional dependency:

```go
type KeyResolver interface {
    // Allocate generates an FK, wraps it for the owner derived from the
    // storage key, persists the row set, and returns (keyUUID, FK).
    Allocate(ctx context.Context, storageKey string) (keyUUID [16]byte, fk []byte, err error)
    // Resolve unwraps the FK named by keyUUID (owner's row preferred) via
    // the master→UK→FK chain.
    Resolve(ctx context.Context, keyUUID [16]byte) (fk []byte, err error)
}
```

Nil resolver → decorator behaves exactly as today (v1/v2 only). Per-user
mode is opt-in: `encryption.per_user_keys: true`. When off, writes stay
bit-identical to the current build — the ADR-0074 rollback carve-out
extends verbatim: a deployment that never enables the mode can roll back
the binary safely; once v3 files exist, rollback strands them (documented,
same as v2).

### 3. Schema (new migration)

- `user_keys(user_id PK, sealed_uk BLOB, key_id INT, created_ms INT)` —
  `sealed_uk` is the UK under the master key with its ring ID.
- `file_keys(key_uuid BLOB, user_id INT, wrapped_fk BLOB, created_ms INT,
  PK(key_uuid, user_id))` — one row per (file key, recipient).
- `files.key_uuid BLOB NULL` — indexes a file to its key UUID (NULL =
  v1/v2 HMAC file); lets the sharing path find the keyUUID from the
  filecache row without parsing sealed headers.

### 4. Sharing integration

- Grant (share created/accepted, user or group member): `WrapFor(fileID,
  uid)` inserts the recipient's `file_keys` row. Group membership changes
  wrap/unwrap per member.
- Revoke (unshare / member removed): delete the recipient row — with
  server-held UKs this is policy-level enforcement (see threat model); it
  becomes cryptographically real only in phase 4.
- Readers never notice: DAV GET resolves through the owner row regardless
  of which sharee triggered the read. Public links, trash, versions,
  previews, and the CLI importer are all server-side readers and keep
  working unchanged.

### 5. Admin recovery

Built into the hierarchy rather than a separate escrow feature: the master
key opens every UK, which opens every FK. `ncgo-cli encryption status`
gains per-user-key reporting (mode, users holding UKs, wrapped-file count,
orphan check: files whose keyUUID has no resolvable row). A
`recovery`-style user-data export can be built later on this chain without
new crypto.

### 6. Implementation phases (each its own increment, full gates)

1. **Envelope + resolver + owner wrap**: v3 header, KeyResolver, schema,
   config flag, owner-only wraps on write; reads/Resolve; sweep direction
   four (`SweepRekeyV3`, reusing the ADR-0070/0074 engine) for eager
   migration, lazy re-seal on write otherwise.
2. **Share wrap/revoke**: sharing-service hooks, group member join/leave,
   unshare row deletion, concurrency with the ADR-0094 write path pinned
   by tests.
3. **Lifecycle + tooling**: UK at user creation, deletion cleanup,
   `encryption status` reporting, rotation re-sealing UK rows.
4. **Password-wrapped UKs (opt-in, threat-model change)**: UK additionally
   sealed under a password-derived KEK (argon2id — first new dependency,
   its own ADR); unlocked into the session at password login;
   app-password/token sessions carry no password and cannot unlock — those
   clients must use a session that had a password login first, or the
   deployment stays on master-wrapped UKs. ~~This constraint and the
   trade-off table belong to that phase's ADR, not this one.~~
   (**design landed in ADR-0100** — enrollment model, X25519 keypairs for
   the lock problem, session/token unlock, schema, and the trade-off
   table; code per phase)

### Alternatives considered

- **Per-user sealing at the storage decorator** — impossible: shared
  objects are stored once (owner key); the decorator would have to pick one
  recipient's key and lock out the rest. Rejected.
- **Asymmetric user key pairs (RSA/X25519)** — justified only by
  client-side wrapping (E2E), which server-side decryption never uses;
  symmetric UKs are simpler, smaller, and sufficient for every phase here.
  Revisit if an end-to-end design ever lands. Rejected for now.
- **keyless v3 header (try-all wraps per open)** — O(recipients) unwrap
  attempts per `Open` and the ADR-0074 ambiguity problem (wrong-key vs
  corrupt) returns at the wrap layer. The 16-byte keyUUID buys O(1)
  lookup. Rejected.

## Consequences

- The encryption format gains v3; v1/v2 readers and writers are untouched
  when the mode is off, and mixed v1/v2/v3 trees are the normal migrated
  state (auto-detect per file, as established in ADR-0052/0074).
- New config: `encryption.per_user_keys` (bool, default false); new tables
  `user_keys`, `file_keys`; new nullable `files.key_uuid`.
- Phase 4 is the only phase that changes the security posture, and the
  only one that constrains token-authenticated clients; it is gated behind
  its own ADR and flag. ~~(its ADR pending)~~ (**ADR-0100** supplies the
  phase-4 design)
- This ADR closes ADR-0074's per-user-keys deferral **as a design**; code
  lands per phase with the standard gates (full suite, race, lint, tidy).

## Verification (design level)

- Each implementation phase ships unit + integration coverage per the
  repo's gate convention: phase 1 pins v3 round-trips, resolver
  Allocate/Resolve, mixed-tree reads, sweep rekey, and rollback bit-compat
  with the mode off; phase 2 pins wrap-on-grant/unwrap-on-revoke and group
  membership churn; phase 3 pins lifecycle and rotation re-seal; phase 4
  gets its own verification section.
