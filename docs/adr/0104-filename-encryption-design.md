# ADR-0104: Server-side filename encryption — parent-keyed deterministic names with ciphertext-materialized paths (design)

- **Status**: Accepted (design; implementation phased as below)
- **Date**: 2026-09-27
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0052 (§6's accepted metadata leakage narrows: names and
  directory structure leave the plaintext set once the mode is enabled),
  ADR-0074 (supplies the filename-encryption design it deferred)

## Context

ADR-0052 §6 accepted plaintext file names, directory structure, mtimes, and
sizes; filename encryption has been a listed follow-up ever since, deferred by
ADR-0074 because *"it changes every storage consumer (filecache paths, DAV
listings, search, shares) and needs its own design phase."* This is that design
phase. It lands now because Phase 5w (ADR-0096 → ADR-0102) built the wrapping
infrastructure the feature needs: per-file keys, per-user keys, per-recipient
wrap rows, and session/token unlock. Names are the last large at-rest leak: a
DB-only exposure (backup theft, SQL injection, leaked dump) today reveals every
path even though file *content* stays sealed under the master key, and for
enrolled users (ADR-0100/0101) an at-rest compromise with no active session
protects content but still exposes names like `2026-layoffs.xlsx`.

Verified facts that shape the design:

- **All subtree machinery is path-string based.** `files` carries a
  materialized full `path` with `UNIQUE(user_id, path)`; `RenameSubtree` /
  `DeleteSubtree` / `ListSealedSubtree` match `path = ? OR path LIKE ?/%`;
  `file_locks.file_path`, `trash_items.original_path`/`name`,
  `file_versions.file_path` (`UNIQUE(user_id, file_path, revision)`), and
  `shares.file_path` all store the same path strings and use the same prefix
  matching. Any design that breaks the prefix property rewrites five stores.
- **Folders carry no keys today**: `files.key_uuid` is set only for files with
  sealed content (ADR-0097/0098); directory rows have NULL.
- **MySQL caps the path column**: `path VARCHAR(768)` utf8mb4 = 3072 bytes, the
  InnoDB index maximum; `name VARCHAR(255)`. SQLite/Postgres are unbounded
  TEXT. Default collations diverge: MySQL is case-insensitive
  (`utf8mb4_0900_ai_ci`), SQLite/Postgres case-sensitive — so name-equality
  behavior already differs by dialect.
- **Incoming share mounts are virtual.** `ListIncoming` (sharing + OCM)
  synthesizes `Mount = "/" + pathBase(owner file_path)` per request; no
  sharee-side `files` row exists. There is no sharee-renamable mount name.
- **No stored displayname**: PROPFIND `displayname` is `files.name`; plugin
  custom props are computed live (ADR-0046), not persisted per file.
- **Trash keeps no key reference**, yet trash restore works for v3 content only
  because `file_keys` rows are keyed by `key_uuid` with no FK to `files` and
  therefore *survive* file deletion implicitly. Permanent deletion orphans
  those wrap rows until `encryption reconcile` prunes them (ADR-0099).
- **Activity/notification rows embed names and paths** in `subject*` and
  `subject_rich_parameters` JSON (migration 0012). Upload sessions carry a
  plaintext destination path for the session's lifetime.
- The wire contract is plaintext URLs: clients address resources by
  human-readable path, and the server must resolve them. This forces
  *deterministic* encryption (or an index); random-per-write names would make
  `GET /Photos/cat.jpg` unresolvable without a full scan.

## Decision

Opt-in **`encryption.filename_encryption: true`** (default false), valid only
with `encryption.per_user_keys: true` (config validation fails startup
otherwise). The mode is per-user with whole-tree cutover; mixed fleets are the
normal migrated state. Construction name: **NCGOFN1**.

### 1. Directory keys (DK)

Every directory row — including the user root — gains key material: a random
32-byte DK addressed by `files.key_uuid`, wrapped per recipient in the
**existing `file_keys` table** (DKs and FKs share the key-uuid namespace and
every wrap/unwrap/lifecycle hook from ADR-0096…0099). DKs are minted at folder
creation and at cutover, wrapped at share grant, unwrapped at revoke, resealed
on master rotation, and inventoried/pruned by `reconcile` exactly like FKs.
The resolver (`internal/storage/encrypt` `KeyResolver`) is reused unchanged —
a DK *is* an FK whose file row is a directory.

### 2. Name construction (byte-exact)

For a child of the folder whose key UUID is `parentKeyUUID`:

```
NK    = HKDF-SHA256(ikm = DK_parent, salt = "", info = "NCGONK1" || parentKeyUUID)   // 32 B
nonce = HMAC-SHA256(NK, "NCGOFN1" || 0x00 || nameBytes)[0:12]                        // synthetic IV
blob  = nonce || AES-256-GCM(NK, nonce, nameBytes, ad = "NCGOFN1" || parentKeyUUID)  // 12 + N + 16 B
token = base64.RawURLEncoding(blob)
```

- **Deterministic**: the same name under the same parent yields the same
  token. This is what preserves the entire SQL surface — `UNIQUE(user_id,
  path)` keeps enforcing name collisions, request-time path resolution
  re-encrypts segment-by-segment into an exact `WHERE user_id = ? AND
  path = ?` lookup, and every prefix operation below works verbatim.
- **Equality leak is bounded to what the schema already leaks**: identical
  names within one folder produce identical tokens (already visible via the
  uniqueness constraint); across folders the parent-keyed NK differs, so
  cross-folder name equality is hidden.
- **SIV-style synthetic nonce**: nonce reuse requires an identical (NK, name)
  pair, i.e. the same row — deterministic AEAD semantics, no misuse window.
  The AD binds the token to the parent key UUID, so a token replayed under a
  different folder fails authentication.
- Byte-oriented: no Unicode normalization, no case folding — name bytes pass
  through exactly. Equality becomes case-sensitive byte equality on **all**
  dialects (tokens compare binary); this aligns MySQL with the
  SQLite/Postgres behavior and is documented as a behavior change for MySQL
  deployments that enable the mode.
- Root is never encrypted: `path "/"`, `name ""` stay literal.

### 3. Storage layout: ciphertext-materialized paths

`files.name` stores the basename token; `files.path` stores `/` + tokens joined
by `/`. Column types are unchanged. Because the materialized path is just
opaque tokens joined by `/`, `RenameSubtree`, `DeleteSubtree`,
`ListSealedSubtree` (which already selects folder rows once folders carry
`key_uuid IS NOT NULL`), `file_locks`, `file_versions`, `trash_items`, and
`shares.file_path` keep working **bit-for-bit** — prefix rewrite, prefix
delete, exact match, no code-path forks.

Migration 0023 (three dialects):

- `files.name_scheme TINYINT NOT NULL DEFAULT 0` — 0 plaintext, 1 = NCGOFN1.
  Self-describing rows permit dual-read and make partial states observable.
- `users.name_scheme TINYINT NOT NULL DEFAULT 0` — the authoritative per-user
  write-path switch (new creates encrypt iff the owner's tree is scheme 1).

**Length budget (pinned)**: plaintext names are limited to 255 characters
(rune count — MySQL `VARCHAR(255)` parity, now enforced explicitly at the DAV
boundary for all dialects), and the computed ciphertext `path` is limited to
**768 characters** (the MySQL index ceiling, enforced on all dialects for
uniformity). Over-budget creates/renames/moves fail with `400` and a clear
message. Consequence: ~2.4× less headroom for deep trees with long names
(e.g. ten 30-byte segments ≈ 790 ciphertext chars is over budget; nine fit).
Deployments needing pathological depth stay on plaintext names (the mode is
opt-in) or wait for the parent_id-recursion alternative (below).

### 4. Request flow and listing order

The wire contract is unchanged — clients send and receive plaintext paths.
The files service gains a translation seam at the store boundary: resolve the
requester's wraps (session `UnlockedKey` for enrolled users, master→UK chain
otherwise — exactly the content path), derive NK per path segment with a
per-request folder-key cache (O(depth) HKDF+GCM per resolution), and issue the
unchanged SQL against ciphertext strings. Listings decrypt names in Go after
`ListChildren` and **sort post-decryption by plaintext name bytes** — `ORDER
BY path` on tokens is meaningless and is removed for scheme-1 users, which
also unifies listing order across dialects. `displayname` is the decrypted
name.

### 5. Subtree semantics

- **Rename within a parent**: re-token the basename; descendants' `path`
  strings get the same prefix rewrite as today. Their *name tokens are
  unchanged* — a folder's DK does not change when the folder is renamed.
- **Move across parents**: re-token the moved entry under the destination
  parent's DK; descendants untouched (their names are keyed to their own
  parent, not to ancestors). O(1) crypto + string prefix rewrite.
- **Copy**: copies mint fresh keys (content copies already allocate new FKs);
  copied folders get new DKs and the copied subtree's names are re-tokenized
  under them. O(subtree) crypto on top of the existing O(subtree) I/O.
- **New file/folder inside a shared folder**: the existing wrap-on-write hook
  (ADR-0098) extends to folder rows — the recipient set of the enclosing
  share gets the new DK wrap, closing the availability gap symmetrically for
  names and content.

### 6. Trash, versions, locks, uploads

- **Trash**: `trash_items.original_path`/`name` store ciphertext strings
  (computed with the deleter's wraps at delete time). Deleting to trash
  **retains** the subtree's `file_keys` wrap rows — today's implicit
  orphan-survival becomes an explicit contract — so trash listing and restore
  resolve. Restore re-tokens the basename under the *current* parent DK.
  **Permanent deletion** (purge/expiry) now deletes the subtree's wrap rows
  explicitly, closing today's wrap-orphan leak; `reconcile` remains the
  safety net.
- **Versions**: `file_versions.file_path` carries the ciphertext path and is
  rewritten by the same rename/move pass; version content keys are unchanged.
- **Locks**: `file_locks.file_path` stores ciphertext paths, lock-null rows
  included (the path is computable before the file exists — determinism
  again). Prefix listing and rename rewrite work verbatim.
- **Upload sessions**: the destination path is stored tokenized at session
  creation (the creator is authenticated and unlocked in every supported
  flow, ADR-0102 included), removing the current plaintext window.

### 7. Sharing, federation, public links

- `shares.file_path` stores the ciphertext owner path; exact-match resolution
  is unchanged. Grant wrapping covers folder DKs via `ListSealedSubtree`
  (no query change — folders now satisfy `key_uuid IS NOT NULL`).
- `ListIncoming` decrypts the mount basename per request via the sharee's own
  wrap rows. Mounts stay virtual: nothing sharee-side is persisted, so there
  is nothing to encrypt on the sharee side. Sharee renames of mounts do not
  exist in ncgo (mount name derives from the owner path), so no design is
  needed there.
- **Public links**: master-wrapped deployments resolve through the owner's
  rows as today; an enrolled owner without an unlocked session yields
  `ErrKeyLocked` → 403 — the same boundary as content (ADR-0101).
- **OCM**: the local side is identical to a local share; the remote peer's
  storage is its own affair. Federated mount names are derived locally and
  follow the local user's scheme.

### 8. Search

`SearchByName`'s SQL `LIKE`/`ILIKE` cannot match tokens. For scheme-1 users
search becomes **scan-and-decrypt**: stream the user's resolvable rows,
decrypt names in Go, apply a Unicode case-folded contains-match (superset of
today's dialect behavior — SQLite ASCII-insensitive, MySQL/Postgres
insensitive — now uniform), apply `LIMIT` after filtering. Documented bound:
~10k rows scan in tens of milliseconds (AES-GCM runs at GB/s). N-gram blind
indexes were rejected: they leak token frequency, add write amplification,
and buy nothing at personal-cloud scale.

### 9. Activity and notifications

File names/paths inside `subject`/`subject_rich_parameters` are written as the
**ciphertext tokens copied from `files.name`/`path` at event time**, marked
with `"ncgoNameScheme": 1` in the parameters JSON. Rendering decrypts with the
viewer's wraps; unresolvable entries (file permanently deleted and wraps
pruned, viewer never had access) render a localized *"encrypted file"*
placeholder — matching how upstream renders dead file references. Actors,
verbs, and timestamps stay plaintext: the activity stream remains an audit
log, and that residual is accepted and documented.

### 10. Threat model and residual leaks

- **Master-wrapped mode**: a DB-only exposure no longer reveals names or tree
  contents (DKs are wrapped under UKs under the master key). The running
  server remains trusted, as in ADR-0052.
- **Enrolled mode (ADR-0100/0101)**: at-rest compromise with no active
  unlocked session now protects names as well as content; the unlock
  boundaries (public links, background jobs, token-less sessions) are exactly
  the content boundaries.
- **Explicit non-goals / residuals**: sizes, mtimes, etags, permissions, tree
  shape (depth, fan-out), duplicate-name equality within a folder, activity
  actor/verb metadata, and **request URLs in server access logs** (ops
  concern — log redaction is a deployment configuration, out of scope). The
  ownerless `appdata_<instance>` system tree keeps plaintext names (its paths
  are pseudonymous fileids already). E2EE pass-through is unaffected.

### 11. Rollout, migration, rollback

- `ncgo-cli encryption encrypt-names [--user uid] [--dry-run]`: per-user,
  one transaction per user — load the tree, mint DKs root-first, compute all
  tokens, rewrite rows, flip `users.name_scheme`. Idempotent; users with
  scheme 1 are skipped. New users created while the mode is on start scheme 1
  (root DK minted in `EnsureRoot`).
- `ncgo-cli encryption decrypt-names` reverses a user (decommissioning /
  rollback preparation), same transaction shape.
- `encryption status` reports scheme-1 users and tokenized-row counts;
  `reconcile` inventories folder DK wraps alongside FK wraps.
- **Rollback carve-out (verbatim from ADR-0096)**: a deployment that never
  enables the mode is bit-identical to the previous build and can roll back
  safely; once any user's tree is scheme 1, an older binary cannot resolve
  those paths — run `decrypt-names` first. Documented in the command help
  and config reference, same as v2/v3 content.

### 12. Implementation phases (each its own increment, full gates)

1. **Primitives + folder keys**: NCGOFN1 token enc/dec with test vectors,
   migration 0023, DK minting behind the flag, KeySharer folder coverage,
   resolver inventory extension. No behavior change with the flag off.
2. **Store + DAV cutover**: translation seam, per-request key cache, listing
   sort, search scan, locks/trash/versions string carry, rename/move/copy
   rules, length-budget enforcement.
3. **Sharing + activity + uploads**: ListIncoming decryption, wrap-on-write
   for folders, activity token subjects, upload-session tokenization,
   public-link boundary tests.
4. **Tooling**: encrypt-names/decrypt-names sweeps, status/reconcile
   reporting, config reference and docs.

### Alternatives considered

- **Per-file name keys + scan-resolve** (random nonces, no determinism):
  every path lookup becomes list-children-and-decrypt per segment; exact-path
  SQL, prefix rewrites, locks, trash, and share strings all break. Rejected.
- **Random nonce + blind-index column** (`HMAC(indexKey, name)` for lookup):
  the index key must be shared by everyone who resolves the tree, adding a
  third wrapped key per recipient per tree plus a new indexed column, for no
  gain over synthetic-IV determinism. Rejected.
- **Parent_id recursion, no materialized path**: recursive CTEs for every
  subtree op, `UNIQUE(user_id, parent_id, name)` — removes the 768-char
  budget but rewrites five stores and cannot represent lock-null paths.
  Documented as the revisit trigger if a deployment needs deep trees under
  MySQL. Rejected for now.
- **Upstream-style metadata files (E2EE app)**: client-side encryption with
  server-opaque metadata — a different product that forfeits server-side
  search, previews, and sharing UX. Rejected (and already covered by the
  E2EE pass-through stance, ADR-0052 §7).
- **AES-SIV / AES-GCM-SIV from a new dependency**: the project's
  zero-new-dependency gate stands; HMAC-derived synthetic nonce + AES-GCM is
  the standard equivalent construction with stdlib+x/crypto primitives
  already vendored. Rejected.
- **One name key per user tree (no folder keys)**: moving a folder would
  re-tokenize every descendant, and sharing a subtree would hand over the
  whole tree's names. Folder keys are what make moves O(1) and shares
  subtree-scoped. Rejected.
- **Encrypt `name` only, leave `path` plaintext**: leaks every ancestor name.
  Rejected.

## Consequences

- New config: `encryption.filename_encryption` (bool, default false; startup
  fails if set without `per_user_keys`). Migration 0023: `files.name_scheme`,
  `users.name_scheme` (three dialects). Folder rows begin carrying
  `key_uuid` + `file_keys` wraps — `ListSealedSubtree` and `reconcile`
  inventory cover them without query changes.
- Behavior alignments, all documented: name equality becomes case-sensitive
  on MySQL when the mode is on; listing order is bytewise on plaintext names
  on all dialects; search matching becomes Unicode case-fold everywhere.
- New enforced limits: 255 runes per name (all dialects), 768 ciphertext
  chars per path, `400` on violation. This is the first hard depth×name
  budget; the config reference states it.
- Trash wrap retention becomes a contract and permanent deletion gains
  explicit wrap pruning — a small security fix piggybacked on phase 2/3.
- Cost per request: O(depth) HKDF+GCM for resolution, O(children) decrypt +
  Go sort per listing, full-tree scan for search. All bounded and documented.
- Rollback: bit-identical when never enabled; `decrypt-names` required to
  decommission once enabled.
- This ADR closes ADR-0074's filename-encryption deferral **as a design**;
  code lands per phase with the standard gates (full suite, race, lint,
  tidy).

## Verification (design level)

- Phase 1 pins NCGOFN1 test vectors byte-exact (fixed DK/name → fixed token),
  determinism, AD binding (wrong-parent rejection), and zero behavior change
  with the flag off (bit-identical writes).
- Phase 2 pins dual-read mixed trees, exact-path resolution through the seam,
  post-decrypt sort order, rename-token stability, cross-parent move O(1)
  re-tokening, copy re-keying, budget-400s, and ciphertext-string behavior of
  locks/trash/versions.
- Phase 3 pins sharee mount decryption, wrap-on-write for new folders under
  shares, public-link 403 for locked enrolled owners, activity placeholder
  fallback, and tokenized upload sessions.
- Phase 4 pins sweep idempotence, per-user transactional cutover (failure
  leaves the user fully scheme 0), decrypt-names round-trip, and
  status/reconcile output.
