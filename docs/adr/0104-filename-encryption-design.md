# ADR-0104: Server-side filename encryption — parent-keyed deterministic names with ciphertext-materialized paths (design)

- **Status**: Accepted (design; implementation phased as below) (phases 1–2 landed 2026-09-27 — primitives, migration 0023, folder key minting, share coverage; store + DAV cutover with the translating decorator, satellite stores, search scan, and the write switch; phase **3a** landed 2026-09-27 — migration 0024, ciphertext share paths with share-root anchoring, KeySharer ciphertext rewiring, public-link boundary; phase **3b** landed 2026-09-27 — share-notification token subjects with list-time decrypt/rebuild, upload-session destination tokenization; phase 4 pending)
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
  (computed with the deleter's wraps at delete time). `location_id` embeds
  only the (200-char-capped) name token — the translator supplies it through
  a `Trash.LocationNamer` seam consulted *before* the storage move, so the
  row id and the trash object keys agree and neither carries the plaintext
  name; the `.d<ts>` suffix and `-N` dedup contract is unchanged, and the
  cap keeps the id within `ValidLocationID`'s 255 chars. (Same-day phase-2
  addition: the leak was found at phase-2 acceptance.) Deleting to trash
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
  flow, ADR-0102 included), removing the current plaintext window. (Landed in
  phase 3b via `TranslatingUploadStore` over the same translation core:
  `Create`/`UpdateDest` tokenize, `Get` decrypts, `Delete` passes through;
  the destination leaf need not exist but its parent MUST — a missing parent
  fails loudly, never a plaintext fallback. Assembly (`uploads.go`) is
  unchanged: `Get` returns plaintext, so the destination comparison and the
  write both run on the translating views.)

### 7. Sharing, federation, public links

- `shares.file_path` stores the ciphertext owner path; exact/prefix matching
  is preserved by the token-join shape, and grant wrapping covers folder DKs
  via `ListSealedSubtree` (no query change — folders now satisfy
  `key_uuid IS NOT NULL`). Phase 3a adds two sealed columns (migration 0024):
  **`mount_name_enc`** — a share-scoped copy of the mount basename as an
  NCGOFN1 token under the share target's OWN key — and **`abs_path_enc`** —
  the plaintext absolute owner path sealed by `SealPath` (NCGOSP1: random
  nonce || AES-256-GCM, ad `"NCGOSP1"||keyUUID`) under the same key. Both
  exist because of two gaps sharees can never close from the tree alone: the
  basename's tree token lives under the share root's PARENT directory key
  (no sharee wrap), and content ops through a mount need the plaintext
  absolute path for storage-key derivation while an enrolled offline owner's
  ancestor chain is unresolvable to anyone. Exactly the wrap holders
  (sharees + server) can open them.
- Name resolution through an incoming mount is **anchored at the share
  root**: `CipherPathUnder`/`DecryptUnderAnchor` walk tokens starting from
  the anchor row's own key and never touch rows above it, so a sharee
  resolves an ENROLLED owner's shared subtree through their own wrap rows
  (lifting phase 2's documented 403 residual); an enrolled sharee without an
  unlocked session stays `ErrKeyLocked` → 403, the ADR-0101 boundary.
  Mounts stay virtual: nothing sharee-side is persisted, so there is nothing
  to encrypt on the sharee side. Sharee renames of mounts do not exist in
  ncgo (mount name derives from the owner path), so no design is needed
  there. **Rename/move re-seal**: the existing `RenamePath` prefix rewrite
  moves the ciphertext `file_path`; a best-effort `ResealShareMeta` pass then
  opens each affected row (the rename does not change directory keys),
  string-rewrites the plaintext prefix inside `abs_path_enc`, re-seals, and
  re-seals `mount_name_enc` when the share's own root basename changed.
  **Reshares** stay rejected (ADR-0017's reshare bit is off): Create always
  seals against the creator's OWN tree, so a mount path (virtual, no local
  row) now fails loudly instead of persisting an orphaned row.
- **Public links**: master-wrapped deployments resolve through the owner's
  rows as today (the anonymous ctx opens the sealed metadata); an enrolled
  owner without an unlocked session yields `ErrKeyLocked` → 403 on both the
  public DAV jail and the direct `/s/{token}` download — the same boundary
  as content (ADR-0101).
- **OCM**: the local (outgoing) side is identical to a local share. OCM
  **incoming** (remote) mount names stay plaintext in `ocm_incoming` (the
  name is the peer's data, chosen at accept time; there is no local DK chain
  for remote content) — recorded as an accepted residual (same-day phase-3a
  amendment; earlier text said "follow the local scheme").

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

**Activity**: the activities table has no production writer yet (store + OCS
read API only), so the token-subject rule below is forward-pinned for the
first activity producer: names/paths inside `subject`/
`subject_rich_parameters` are written as ciphertext tokens with the
`"ncgoNameScheme"` marker, and rendering decrypts with the viewer's wraps.

**Notifications** (landed phase 3b): the one live subject producer is the
share bell (`notifyShareCreated`, user and group shares). For a scheme-1
owner the stored subject embeds the mount-name **token** — the share target's
basename as an NCGOFN1 token under the target's own key, byte-identical to
`shares.mount_name_enc` (deterministic) — never the full owner path: a sharee
cannot resolve ancestor names, so the rendered display name is the mount
basename, a documented display change for encrypted trees. The row adds a
`"ncgoNameScheme"` meta param (`{"type":"ncgo","id":<key UUID
hex>,"name":"1"}`) naming the sealing key; the param is never referenced by
the rich template. The OCS list/get render strips the marker from the emitted
params, decrypts the token in the **viewer's** ctx (an enrolled sharee
resolves through their own wrap row; keyless → `ErrKeyLocked`), and
**rebuilds the plain subject from the SubjectRich template** by substituting
each `{key}` with the post-decryption param name. ANY decrypt failure —
locked keys, tampered token, pruned wraps — degrades the item to the constant
*"encrypted file"* placeholder: per item, never failing the list, never
emitting the token (matching how upstream renders dead file references).
Scheme-0 rows and unmarked rows pass through verbatim, as does every row with
the seam unwired (flag off). Actors, verbs, and timestamps stay plaintext:
the stream remains an audit log, and that residual is accepted and
documented. The admin console's notifications panel (ADR-0090) ships the
stored subject to the admin UI verbatim — an admin-audience view where a
tokenized subject displays as the token.

### 10. Threat model and residual leaks

- **Master-wrapped mode**: a DB-only exposure no longer reveals names or tree
  contents (DKs are wrapped under UKs under the master key). The running
  server remains trusted, as in ADR-0052.
- **Enrolled mode (ADR-0100/0101)**: at-rest compromise with no active
  unlocked session now protects names as well as content; the unlock
  boundaries (public links, background jobs, token-less sessions) are exactly
  the content boundaries. (Phase-3a note: `shares.file_path` left the leak
  set for scheme-1 owners — it carries ciphertext plus the AEAD-sealed
  `mount_name_enc`/`abs_path_enc` copies; the share grant no longer publishes
  the owner path to a DB-only reader.)
- **Explicit non-goals / residuals**: sizes, mtimes, etags, permissions, tree
  shape (depth, fan-out), duplicate-name equality within a folder, activity
  actor/verb metadata, and **request URLs in server access logs** (ops
  concern — log redaction is a deployment configuration, out of scope). The
  ownerless `appdata_<instance>` system tree keeps plaintext names (its paths
  are pseudonymous fileids already). E2EE pass-through is unaffected.
  **Storage-backend object keys (localfs paths, S3 keys) stay plaintext** —
  content is sealed but names are visible to a backend-level attacker,
  exactly as upstream Nextcloud SSE; obfuscating object keys is a separate
  design, out of scope. (Residual added same-day with the phase-2 cutover:
  the filecache is ciphertext while the backend layout is not.)

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
   migration 0023, DK minting behind the flag, folder wrap-on-write for new
   folders under covering shares, KeySharer folder coverage, resolver
   inventory extension. No behavior change with the flag off. (Phase
   assignment refined same-day: folder wrap-on-write landed in phase 1 — the
   mint site holds the DK, so one WrapForWrite call closes the gap
   immediately.)
2. **Store + DAV cutover**: translation seam, per-request key cache, listing
   sort, search scan, locks/trash/versions string carry, rename/move/copy
   rules, length-budget enforcement. (Landed 2026-09-27, with these pinned
   refinements, all same-day:
   - **One translating decorator** around the files `Store`
     (`files.TranslatingStore` over `SQLStore`) plus thin wrappers reusing
     the same core for the path-keyed satellite stores (`file_locks`,
     `trash_items`, `file_versions`). DAV and every other consumer keep
     speaking plaintext paths; only DB rows carry ciphertext. Flag off = no
     wrapper = bit-identical (the untouched golden DAV suite is the pin).
   - **The key/row caches are strictly per top-level store call** — never a
     shared or global NK/DK cache. `Resolve` enforces per-ctx authorization
     (an enrolled reader resolves through their own wrap rows,
     ADR-0100/0101), so a cross-request cache would let an unauthorized
     reader decrypt names out of another request's key material.
   - `shares.file_path` **stays plaintext until phase 3**: the KeySharer
     flows through the translating store with plaintext share paths (no
     double-encryption), and incoming-share mounts derive from the plaintext
     share row as today. Consequence: a sharee cannot name-resolve an
     *enrolled* owner's tree in phase 2 — the share-root's ancestor DKs are
     not wrapped for sharees, so the read is `ErrKeyLocked` → 403 (the
     ADR-0101 boundary, now covering name resolution). Phase 3's ciphertext
     `shares.file_path` anchors resolution at the share root and lifts this.
   - **Importer/CLI-created trees stay scheme 0** until the phase-4
     encrypt-names sweep: only the server wiring flips `users.name_scheme`
     at user creation. The bootstrap admin (created before the wiring) also
     stays scheme 0 until the sweep.
   - **Search unifies case-folding**: scheme-1 search is scan-and-decrypt
     with a Unicode case-folded contains (`strings.ToLower` on both sides),
     superseding the sqlite-ASCII/pg-ILIKE divergence; the limit applies
     after filtering and results sort by (name, id).
   - The trash listing degrades a trash item whose ancestor rows are gone
     (a child trashed before its parent) to its stored ciphertext fields —
     the §9 placeholder philosophy, never fabricated plaintext — while a
     token that fails authentication stays a hard `ErrIntegrity`.
   - `trash_items.location_id` embeds the (200-char-capped) name token via a
     `Trash.LocationNamer` seam consulted before the storage move — the row
     id and trash object keys carry no plaintext name, and the cap keeps the
     id within `ValidLocationID`'s 255 chars. Scheme-0 users keep the
     plaintext basename form bit-identically.)
3. **Sharing + activity + uploads** — split same-day into two increments.
   **3a (landed 2026-09-27) — sharing**: ciphertext `shares.file_path` with
   the §7 grant-time sealed copies (migration 0024: `mount_name_enc`,
   `abs_path_enc`), share-root-anchored mount resolution (`CipherPathUnder`/
   `DecryptUnderAnchor`/`AnchorRow`) lifting the phase-2 enrolled-sharee 403,
   `ListIncoming` opening mounts in the sharee's ctx, owner-facing OCS views
   opening them in the owner's ctx, the rename re-seal pass, the KeySharer
   rewired to ciphertext paths end to end (raw meta seam; `WrapForWrite` call
   sites translate), and the public-link master-works/enrolled-403 boundary.
   **3b (landed 2026-09-27)** — notification token subjects + upload-session
   destination tokenization. The share bell (`notifyShareCreated`, the only
   live §9 producer — the activities table has no production writer, so the
   activity token rule is forward-pinned) stores the mount-name token
   (basename display for scheme-1 trees), the `"ncgoNameScheme"` key-UUID
   marker param, and a token-embedded subject that the OCS render rebuilds
   from the rich template after decrypting in the viewer's ctx, degrading per
   item to the `encrypted file` placeholder on any failure.
   `TranslatingUploadStore` tokenizes `uploads.destination` (loud failure on
   a missing parent, no plaintext fallback; `uploads.go` unchanged). OCM
   incoming mount names stay plaintext — the phase-3a residual, unchanged.
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
- Phase 3a pins ciphertext share rows (no plaintext path material),
  ListIncoming mount decryption with OwnerPath/OwnerCipherPath, the enrolled
  sharee's end-to-end DAV operability through a ciphertext mount (PROPFIND,
  GET, PUT with both wrap rows, MKCOL DK mint+wrap) plus the no-session 403
  variant, rename re-seal of mount_name_enc/abs_path_enc (root/ancestor/
  below-point), owner OCS plaintext views, and the public-link
  master-works/enrolled-403 pair. Phase 3b pins the share-notification token
  subject (no plaintext path material; token == mount_name_enc), the
  list-time decrypt + template rebuild + marker strip, the `encrypted file`
  placeholder degradation (keyless viewer, tampered token), scheme-0
  verbatim, and tokenized upload sessions (raw ciphertext destination,
  plaintext Get, UpdateDest re-tokenizing, loud missing-parent failure, the
  full chunked assemble flow).
- Phase 4 pins sweep idempotence, per-user transactional cutover (failure
  leaves the user fully scheme 0), decrypt-names round-trip, and
  status/reconcile output.
