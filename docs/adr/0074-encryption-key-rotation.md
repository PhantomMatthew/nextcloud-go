# ADR-0074: Phase 5b server-encryption master-key rotation — key-ID headers and the append-only keyring

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0052 (v2 sealed-file format; resolves its key-rotation
  follow-up), ADR-0070 (the sweep gains a third direction; resolves its
  key-rotation mention)

## Context

ADR-0052 shipped server-side encryption with a single master key and
explicitly deferred rotation; ADR-0070 delivered the re-encode engine (the
sweep) a rotation flow needs. Without rotation, a key that is suspected
compromised — or simply aged past policy — can never be changed without
making every sealed file unreadable: the data key is HMAC-SHA256 of the
master key over the per-file salt, so replacing the key orphans all
content. Operators expect scheduled master-key rotation as a baseline
hygiene capability.

Verified facts that shape the design:

- A GCM authentication failure cannot distinguish *wrong key* from
  *corrupted ciphertext* — both are a tag mismatch surfacing as
  `ErrIntegrity`. Trying every configured key against every chunk is the
  only keyless-header alternative: O(n keys) AEAD attempts per chunk read,
  on the hot path of every `Open`, and a wrong key is still
  indistinguishable from corruption after all n fail. An explicit key
  identifier in the header removes both problems at the cost of one byte.
- The v1 header (ADR-0052) is `"NCGOENC1" || salt(32)`, and its magic's
  last character is free to act as a format-version marker; every read
  path (`Open`, `plainInfo` behind `Stat`/`List`) already sniffs the magic
  before interpreting the header.
- The sweep machinery (ADR-0070) is direction-parameterized and already
  walks the raw backend, reads through the encrypt FS, and rewrites
  through `Create` atomically. A rotation is a re-encode whose *target* is
  "sealed under the current key" — a third direction, not new machinery.
- Deployments that never rotate exist and must not pay for the feature:
  their on-disk format cannot change, or a rollback of the binary would
  strand newly written files behind a format the old binary cannot read.

## Decision

1. **v2 header: version bump + one key-ID byte.**
   `"NCGOENC2" (8B) || keyID (1B) || salt (32B)` — 41 bytes, versus v1's
   40. The last magic character carries the format version; the ID byte
   names which ring key derived the file's data key (HMAC-SHA256
   construction unchanged). v1 files implicitly have key ID 0. Reads
   sniff 9 bytes (magic + ID) to select the header layout, then resolve
   the ID through the keyring; a v2 ID the ring does not hold fails with
   the new sentinel `ErrUnknownKeyID`, wrapped with the ID and path. That
   error is deliberately distinct from `ErrIntegrity`: it reports an
   operator configuration error (a key was removed from the ring), not a
   wrong-key/corruption signal, and the two must never be conflated in
   operator tooling. The alternative — no ID byte, try every key — was
   rejected: GCM failure cannot tell wrong-key from corruption, and
   try-all is O(n keys) per chunk on every read.

2. **Append-only keyring; the current key seals, previous keys only
   read.** `encrypt.FS` replaces its single `masterKey` with `keys
   [][]byte`: positions 0..n-1 are previous keys, position n is the
   current key and the ID written into new v2 headers. Config key
   `encryption.previous_key_paths` (list, default empty) feeds
   `encrypt.NewWithPrevious(current, previous, inner)`; `New(masterKey,
   inner)` remains as the single-key convenience wrapper. Validation at
   both layers (config `Validate` and the constructor): every key exactly
   32 bytes, ring total ≤ 256 (the ID byte's range), no byte-duplicate
   keys (ambiguous IDs are a misconfiguration), no empty/duplicate paths,
   and `master_key_path` may not also appear in the list.
   **The list is append-only forever**: key IDs are positional, so
   reordering or removing an entry re-keys every file sealed under the
   displaced IDs — reads then fail with `ErrUnknownKeyID` (removal) or,
   worse, silently wrong-key `ErrIntegrity` (reorder). Orphaned files are
   the designed consequence of violating the rule, not a bug; keyring
   cleanup/compaction (re-sealing everything to collapse the ring) is
   future work and does not block rotation itself.

3. **Single-key rings write v1 bit-identically; multi-key rings write
   v2.** A deployment with no `previous_key_paths` produces headers
   byte-for-byte identical to pre-rotation builds (`"NCGOENC1" || salt`,
   40 bytes): zero format change, and the binary can be rolled back
   safely. Only a ring that has actually rotated writes the v2 header.
   Mixed v1/v2 trees are therefore the *normal* state during and after a
   rotation — reads auto-detect per file, exactly as the ADR-0052
   plaintext/sealed mixed state was.

4. **Rotation is a third sweep direction, `SweepRotate`.** Target
   encoding: "sealed under the current key ID". `sniff` now returns the
   header's key ID (v1 → 0). Per file: plaintext is **skipped** (rotation
   is not encrypt-all; `encrypt-all` seals plaintext and lands on the
   current key for free, so the two commands compose instead of overlap);
   sealed-under-current is skipped; sealed-under-any-other-ID is read
   through the encrypt FS (auto-detects the header version, picks the
   ring key) and rewritten through `enc.Create` (seals under current,
   fresh salt). Abort-at-first-file semantics extend the ADR-0070 rule: a
   read wrapping `ErrIntegrity` (the ring's key at that ID does not match
   the data — same wrong-key semantics as SweepOpen) or `ErrUnknownKeyID`
   (the ring is incomplete — every remaining old file would fail) aborts
   the run with an error, rather than counting and burying the cause.
   `SweepRotate` on a single-key ring is an error (nothing to rotate).
   Dry-run counts and sums plaintext bytes via `enc.Stat`, like
   SweepOpen. SweepSeal/SweepOpen are unchanged and keep working with a
   keyring FS.

5. **CLI: `encryption rotate-keys`, one keyring for all sweep
   subcommands.** `ncgo-cli encryption rotate-keys [--user uid]
   [--dry-run]` shares the sweep flag/run wiring with encrypt-all and
   decrypt-all and reports in the same style (`rotate-keys: scanned=N
   rotated=M skipped=K failed=F bytes=B`). Guards: `encryption.enabled`
   required, and at least one previous key must be configured — the error
   otherwise prints the full rotation procedure. The keyring is built
   once from config (`encrypt.LoadKeyring`, fail fast naming the
   offending path) and used for **all** sweep subcommands, so an
   `encrypt-all` run during a rotation seals to the current key.
   `encryption status` additionally prints the number of configured
   previous keys, each key file's loadability, and the current key ID (=
   number of previous keys). The parent help documents the procedure:
   (1) `init --key-path <new>`; (2) move the old `master_key_path` into
   `previous_key_paths` (**appended** at the end) and point
   `master_key_path` at the new key; (3) restart — old files keep
   reading, new writes seal under the highest-ID key; (4) `rotate-keys`;
   (5) the list is append-only forever.

## Alternatives Considered

### Envelope encryption (re-wrap headers only)
- Pros: rotation rewrites a small wrapped-key header per file instead of
  re-sealing content.
- Cons: needs AES-KW and a second header layout, and ADR-0052 already
  rejected it for v1 — rotation here reuses the proven sweep engine with
  zero new crypto, and re-sealing costs one pass over data the operator
  touches only per-rotation. Rejected again.

### Try-all-keys reads without an ID byte
- Pros: no format change at all; v1 files readable by any ring.
- Cons: O(n keys) GCM attempts per chunk on every read, and after all n
  fail the error still cannot distinguish "key retired" from "corrupt
  file" — the two operator actions (restore the key vs. restore from
  backup) are opposite. The ID byte makes both the lookup O(1) and the
  error explicit (`ErrUnknownKeyID`). Rejected.

### Forbidding rollback by always writing v2
- Pros: one header layout on the write path.
- Cons: every non-rotating deployment's format changes the day the binary
  upgrades, and downgrading strands the files written in between. The
  single-key→v1 carve-out keeps the change opt-in per deployment.
  Rejected.

### Re-keying the ring instead of re-sealing files (renumbering IDs)
- Pros: no data movement.
- Cons: IDs are positional and baked into every v2 header; renumbering is
  exactly the reorder hazard the append-only rule exists to prevent.
  Rejected.

## Consequences

- New config surface: `encryption.previous_key_paths` (list, default
  empty); validated (non-blank, unique, master not repeated, ≤255 entries
  so the ring fits 256 IDs). Startup (`app.openStorage`) and the CLI load
  the full keyring fail-fast; a broken previous key is as fatal as a
  broken master key.
- On-disk: v2 files grow the header by one byte (41 vs 40); chunk
  framing, tags, and size math are otherwise unchanged, and
  `Stat`/`List` report plaintext sizes for both versions.
- Removing or reordering `previous_key_paths` entries orphans files by
  design: reads fail with `ErrUnknownKeyID` and `rotate-keys` aborts
  until the key is restored. This is stated in the config docs, the
  command help, and `encryption status` output.
- During a rotation, server writes seal under the new key immediately;
  old files migrate as the sweep reaches them; readers never observe a
  partial file (sweep writes are atomic per ADR-0070).
- `New` is now `NewWithPrevious(masterKey, nil, inner)`; no caller
  changes were required outside the two keyring wiring sites
  (`app.openStorage`, CLI `openStorage`/sweep commands).

### Disposition of remaining encryption follow-ups (Deferred)

- **Filename encryption** — deferred: it changes every storage consumer
  (filecache paths, DAV listings, search, shares) and needs its own
  design phase.
- **Per-user keys** — deferred: sharing requires per-recipient key
  wrapping and an admin-recovery design.
- **S3 SSE-C** — rejected: redundant with the ADR-0052 decorator. The
  SSE-C key would still be server-held (identical threat model), and the
  feature adds per-request header plumbing for no security gain.

## Verification

- `internal/storage/encrypt/keyring_test.go`: constructor validation
  (short/duplicate keys, nil backend, 256-key cap with the 255-previous
  boundary, key-copy immutability); single-key rings (`New`,
  `NewWithPrevious` with nil/empty previous) write v1 headers
  bit-identical to pre-keyring builds (40-byte header, empty file =
  header only); a two-key ring writes v2 with ID byte 1 and round-trips
  multi-chunk content; a ring reads v1 files and plaintext transparently
  with correct `Stat`/`List` sizes; hand-crafted v2-ID-0 read; unknown-ID
  reads fail with `ErrUnknownKeyID` (naming path and ID) and never with
  `ErrIntegrity`, while a known ID with wrong material stays
  `ErrIntegrity`; v2 `Stat`/`List` plaintext sizes across chunk
  boundaries; `LoadKeyring` fail-fast with the offending path named.
- `internal/storage/encrypt/sweep_test.go` (rotate cases): v1 tree +
  plaintext + current-key file rotates to v2 ID 1 with exact stats and
  byte-identical content, idempotent re-run; the append-only rule is
  pinned (the new key alone reads rotated files as `ErrUnknownKeyID`);
  plaintext skipped; dry-run counts plaintext bytes with zero writes;
  aborts on `ErrUnknownKeyID` (shrunk ring) and on `ErrIntegrity` (wrong
  key at a present ID), each at the first file; single-key ring rejected.
- `cmd/ncgo-cli/encryption_test.go`: end-to-end rotation on a localfs
  backend (init A → seal → init B → reconfigure → dry-run → real run with
  exact report lines → v2 ID-1 headers → content intact → idempotent
  re-run → `encrypt-all` afterwards seals to the current key); guard
  errors for disabled encryption and for no previous keys (procedure
  printed); status keyring output (count, per-key OK/UNUSABLE fail-closed,
  current key ID).
- `internal/config`: YAML list parsing via `full.yaml`, native-list
  overrides, empty default, and the validation matrix (blank, duplicate,
  master-repeated, >255 entries).
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` no-op,
  `go test -race` green, encrypt coverage maintained.
