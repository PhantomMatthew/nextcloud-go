# ADR-0052: Phase 4f server-side encryption at rest

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 4 adds the operational-security features expected of a Nextcloud
replacement. PHP Nextcloud ships a server-side encryption app
(`files_encryption`) that seals user content at rest; operators moving to
ncgo need an equivalent guarantee: a compromise of the storage backend
(stolen disk, leaked S3 bucket, rogue object-store admin) must not expose
file contents.

The storage layer is already an interface (`storage.Storage`, ADR-0001)
with two backends (localfs, S3), and every content write in the server
flows through `Storage.Create` streaming the full body (verified in
`internal/files/dav.go`: `DAV.write` calls `Create(ctx, key, 0)` and copies
the entire stream; there is no in-place append path). That makes a
decorator the natural seam: wrap the backend once at startup and every
DAV/files/trash/versions write is sealed without touching callers.

## Decision

1. **A `storage.Storage` decorator, not backend changes.**
   `internal/storage/encrypt.FS` wraps any `storage.Storage` and is wired
   in `app.openStorage` (and the duplicated `openStorage` in
   `cmd/ncgo-cli/deps.go`, so CLI writes such as `import-nextcloud files`
   are sealed identically) when `encryption.enabled: true`. Stat/Open/
   Create/List are translated; Delete/Rename/Mkdir pass through. No
   backend or DAV-layer code changes.

2. **Format: AES-256-GCM, 64 KiB chunks, per-file key derived by
   HMAC-SHA256.** Stdlib `crypto/aes` + `crypto/cipher` only — no new
   dependencies.

   ```
   header  = "NCGOENC1" (8 bytes magic) || salt (32 random bytes)
   dataKey = HMAC-SHA256(masterKey, salt)          // 32 bytes, per file
   chunk i (64 KiB plaintext, last chunk short):
     nonce_i = 0x00000000 || uint64(i) big-endian  // 12 bytes
     sealed_i = AES-256-GCM(dataKey, nonce_i, chunk_i)  // +16-byte tag
   file = header || sealed_0 || sealed_1 || ...
   empty file = header only
   ```

   GCM nonce reuse under one key is fatal, so the nonce cannot be derived
   from the salt: instead the *key* is per-file (HMAC over the random
   salt) and nonces are a simple per-chunk counter, unique per
   (file, chunk) by construction. Chunked framing keeps random access and
   memory bounded (one 64 KiB chunk cached per open file) and localizes
   tamper detection to a chunk. Every chunk read verifies the GCM tag; a
   verification failure returns `ErrIntegrity` and is never swallowed.

3. **Magic auto-detect gives mixed-state operation and a migration path.**
   `Open`/`Stat`/`List` read the first 8 bytes: files carrying the magic
   decrypt transparently (sizes reported are plaintext sizes, computed
   from the stored size minus header and per-chunk tags, so the filecache
   stays consistent); anything else is treated as legacy plaintext and
   passed through untouched. Enabling encryption on an existing instance
   therefore needs no flag day: old files keep working and are sealed the
   next time they are rewritten (DAV writes always go through `Create`).
   The residual risk — a legacy plaintext file whose first 8 bytes happen
   to be `NCGOENC1` — is vanishingly unlikely and accepted; such a file
   would surface integrity errors rather than silent corruption.

4. **Master key: operator-managed file, fail closed.** The master key is
   32 random bytes, base64-encoded (single line) in an operator-created
   file whose path comes from `encryption.master_key_path` (koanf config;
   validation requires the path when `encryption.enabled` is true). The
   server never generates or stores a key by itself. `LoadMasterKey`
   requires the file to decode to exactly 32 bytes and (on non-Windows
   platforms) to have perm bits ≤ 0600; startup fails loudly on any key
   problem — a server that cannot decrypt must not run half-blind.
   `ncgo-cli encryption init` generates the key file (mode 0600, refuses
   to overwrite without `--force`, never prints the key) and
   `ncgo-cli encryption status` reports the enabled state plus key file
   existence/loadability/permissions. Key material never appears in logs
   or error messages.

5. **Threat model: protects the backend, not the server.** Encryption at
   rest defends against compromise of the storage substrate (disks,
   backups, object storage) and against backend-side tampering (GCM
   authentication). It does **not** defend against compromise of the
   running server: the master key lives on the server, plaintext exists in
   process memory during reads/writes, and a hostile server admin can read
   both. Operators needing protection from the server itself must use
   end-to-end encryption (below).

6. **Metadata is not encrypted in v1.** File names, directory structure,
   modification times, and plaintext *sizes* (stored sizes reveal them
   within header+tag overhead) remain visible to the backend. This
   metadata leakage is documented and accepted for v1; filename encryption
   is a listed follow-up.

7. **E2EE pass-through stance.** End-to-end-encrypted folders (where
   clients seal content before upload) are stored as opaque blobs — which
   is exactly what the DAV layer already does. ncgo makes **no server-side
   processing guarantees for E2EE content**: no search indexing, no
   previews/thumbnails (preview generators must skip un-parseable blobs —
   relevant for Phase 4g), no virus scanning. When server-side encryption
   is also enabled, E2EE blobs are simply sealed a second time at rest,
   which is harmless.

## Alternatives Considered

### Whole-file (non-chunked) sealing
- Pros: simpler format, one tag per file.
- Cons: reads and the `io.Seek` contract require decrypting the entire
  file into memory; a single bit flip invalidates everything rather than
  one chunk; large files become a memory/DoS concern. Rejected.

### Per-file random data key wrapped with the master key (envelope)
- Pros: key rotation re-wraps headers only, without re-sealing content.
- Cons: needs a key-wrapping primitive (AES-KW) and header layout for the
  wrapped key; rotation still requires rewriting every header and is not
  implemented in v1 anyway. HMAC derivation is simpler, stdlib-only, and
  equally secure for a fixed master key; rotation is deferred to a
  follow-up that can introduce a key-ID byte in the header. Rejected for
  v1.

### Encrypting inside the DAV/files layer
- Pros: could encrypt names and filecache metadata too.
- Cons: duplicates logic across every caller (trash, versions, previews,
  import CLI), misses the S3/localfs symmetry, and entangles crypto with
  HTTP semantics. The decorator is one seam with total coverage. Rejected.

### Streaming cipher (XChaCha20-Poly1305) via x/crypto
- Pros: no chunking needed, larger nonce space.
- Cons: adds a dependency for marginal benefit; the project constraint for
  this phase is stdlib crypto only. Rejected.

## Consequences

- New config surface: `encryption.enabled` (default false),
  `encryption.master_key_path`. Validation rejects `enabled` without a key
  path; `openStorage` fails startup on any key problem (fail closed).
- All content written while enabled is sealed; reads auto-detect, so
  enabling/disabling never strands data — but **disabling** encryption
  leaves sealed files unreadable (the decorator is what decrypts), so
  operators must treat the key file and the enabled flag as permanent
  once data is sealed. Back up the key file: losing it destroys all
  sealed data irrecoverably.
- Stored files grow by 40 bytes (header) + 16 bytes per 64 KiB chunk
  (~0.024%); `Create`'s size hint is dropped (backends treat it as a
  preallocation hint only — verified in localfs, which ignores it).
- `Stat`/`List` open each listed file to sniff the magic — acceptable
  overhead for v1 (one 8-byte read per file); a future filecache flag
  could memoize encryption state.
- Tampered, wrong-key, or truncated files surface `ErrIntegrity` /
  `io.ErrUnexpectedEOF` on read — loud failures, never silent plaintext.

### Follow-ups (explicit, not in v1)

- ~~**Key rotation** (header gains a key-ID/generation byte; re-seal
  sweep).~~ **Resolved by ADR-0074** (Phase 5b): v2 key-ID header,
  append-only keyring, `ncgo-cli encryption rotate-keys`.
- **Per-user keys** (PHP Nextcloud parity; requires recovery-key design).
- ~~**`encryption encrypt-all` CLI sweep** to seal legacy plaintext files
  in place without waiting for organic rewrites (and a matching
  `decrypt-all` for decommissioning).~~ **Resolved by ADR-0070** (Phase
  4x): `ncgo-cli encryption encrypt-all|decrypt-all [--user uid]
  [--dry-run]`.
- **Filename encryption** to close the metadata-leakage gap.
- **SSE-C for the S3 backend** as an alternative/complement.

## Verification

- `internal/storage/encrypt/encrypt_test.go`: byte-identical round trips
  for sizes 0, 1, 100, 64KiB-1, 64KiB, 64KiB+1, 3*64KiB+17, and 5 MiB;
  `Stat`/`List` report plaintext sizes; empty files store the header only;
  seeks at chunk boundaries, mid-chunk, `SeekEnd`/`SeekCurrent`, past-EOF,
  and invalid whence; legacy plaintext passthrough (content, sizes, mixed
  directories); tamper detection (flipped ciphertext byte, corrupted salt,
  wrong master key → `ErrIntegrity`; mid-chunk and in-header truncation →
  errors); ciphertext uniqueness across files and rewrites (random salt);
  key-file loading (0600 enforced, bad base64/length/missing rejected);
  pass-through of Mkdir/Rename/Delete semantics and error mapping.
- `cmd/ncgo-cli/encryption_test.go`: init writes a 0600 key file that
  loads, never prints key material, refuses overwrite without `--force`,
  `--force` rotates; status reports disabled/enabled/OK/MISSING/UNUSABLE
  and fails closed; enabled-without-key-path is rejected by config
  validation (`internal/config` test).
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` no diff.
