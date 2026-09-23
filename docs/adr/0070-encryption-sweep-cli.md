# ADR-0070: encryption encrypt-all / decrypt-all CLI sweep

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0052 (resolves its `encryption encrypt-all`/`decrypt-all`
  follow-up)

## Context

ADR-0052's server-side encryption is transparent seal-on-write: once
enabled, new writes are sealed, pre-existing plaintext reads pass through,
and any file is sealed the next time it is rewritten. "Wait for organic
rewrites" means a large share of legacy plaintext sits on the backend
indefinitely — exactly the exposure the feature exists to remove — and
there is no way to decommission encryption (turn sealed files back into
plaintext) without losing read access. ADR-0052 therefore listed an
`encrypt-all`/`decrypt-all` sweep as a follow-up; this increment implements
it.

Verified facts that shape the design:

- The storage layout is a single namespace: user content at `<uid>/<path>`
  (`internal/files/dav.go` `storageKey`), system trees at
  `appdata_<instance.id>/...`. Every server write flows through the encrypt
  wrapper (`app.openStorage`), so **any** plaintext file anywhere in the
  tree is legacy residue: the sweep scope is the whole backend, with
  `--user <uid>` scoping to one user's `<uid>/` subtree.
- Both backends install `Create` content atomically (localfs: temp file +
  rename, `internal/storage/localfs`; s3: buffer/spool + single put on
  `Close`, `internal/storage/s3`). There is no partial-overwrite state a
  reader can observe.
- The encrypt decorator auto-detects the encoding per file on every read
  (magic sniff, ADR-0052 §3), so plaintext and sealed files are
  interchangeable to readers at any moment.
- User-visible metadata lives in the filecache and holds *plaintext* logic
  values (mtime, size, etag); the encrypt layer already reports plaintext
  sizes for sealed files, so re-encoding a file changes nothing the
  filecache tracks.

## Decision

1. **Sweep logic in the encrypt package, CLI as thin wiring.**
   `internal/storage/encrypt/sweep.go` exposes
   `Sweep(ctx, raw storage.Storage, enc *FS, opts SweepOptions) (SweepStats, error)`
   with `Direction` (seal/open), `Prefix` ("" = whole backend, `"<uid>/"` =
   one user), `DryRun`, a `Progress` callback, and an `OnError` callback.
   The CLI (`ncgo-cli encryption encrypt-all|decrypt-all [--user uid]
   [--dry-run]`) only loads the config, key, and raw backend and prints
   progress/failures/the summary. Keeping the walk in the package makes it
   unit-testable against localfs without the CLI harness.

2. **Whole-tree scope; `--user` selects a subtree.** `encrypt-all` and
   `decrypt-all` walk the entire default backend by default — user trees,
   `appdata_<id>` system trees, trash, versions — because every byte the
   server stores passes the same wrapper and legacy plaintext can sit
   anywhere. `--user <uid>` restricts the walk to the `<uid>/` prefix (a
   missing prefix is an empty sweep, not an error: a user may have no
   files). The CLI rejects `--user` values containing path separators.

3. **Serial per-file rewrite, one file's content in memory, idempotent.**
   The walk is a serial recursive `List` traversal (the storage interface
   lists one level at a time) bounded by a depth cap against symlink
   cycles. Per file: sniff the first `len(magic)` bytes on the raw backend;
   a file already in the target encoding is skipped (empty files and files
   shorter than the magic count as plaintext); otherwise the file is read
   in full (raw for seal, decrypting for open) and written back through the
   other layer. `Create` replaces the file atomically, so an interrupted
   sweep leaves every file in *some* complete encoding and re-running
   converges — the sweep is freely re-runnable. Serial execution and
   holding at most one file's plaintext keep the resource footprint flat
   and the behavior predictable; parallelism is a non-goal for an operator
   maintenance command (see Alternatives).

4. **Concurrent reads are safe; no downtime required, low traffic
   recommended.** A concurrent reader goes through the server's encrypt
   wrapper, which auto-detects the encoding per open: before the rewrite it
   reads the old encoding, after the atomic rename the new one — never a
   half-written file, and the plaintext content is identical either way.
   The one real hazard is a concurrent *write*: a user rewriting a file
   between the sweep's read and its rename would lose that write (the
   sweep installs the older content, correctly encoded). The window is
   per-file and small, and the same race exists between any two writers —
   but a whole-tree sweep touches files nobody is actively editing, so the
   help text recommends running at low-traffic times rather than mandating
   maintenance mode.

5. **filecache, versions, and etags are deliberately untouched.** The
   sweep changes only the encoding at rest; the plaintext content — and
   therefore size, mtime, checksums, and content-derived etags — is
   byte-identical. The filecache stores plaintext logic values (the encrypt
   layer reports plaintext sizes), so nothing it holds goes stale.
   Versioning records content history, not encoding history: a re-encoded
   file is the same version, and creating a new version per swept file
   would flood version stores with no-op entries. Trash and version blobs
   on the backend are themselves legacy plaintext candidates and are swept
   like any other file.

6. **Per-file failures are counted and skipped; a wrong master key aborts
   immediately.** An unreadable or unwritable file increments `Failed`,
   is reported via `OnError` (the CLI prints `failed: <path>: <err>`), and
   the sweep continues; the summary line reports the counters and the CLI
   exits non-zero when `Failed > 0`. The exception is `ErrIntegrity` while
   decrypting in the open direction: it means the master key does not match
   the sealed data (or the file is corrupt), so every subsequent sealed
   file would fail identically — continuing would only burn time and bury
   the cause. The sweep aborts at the first such file with an error naming
   the key mismatch. (In the seal direction `ErrIntegrity` cannot occur:
   plaintext reads never authenticate.)

7. **CLI contract.** Both subcommands require `encryption.enabled: true`
   and a loadable master key (fail closed, same as the server).
   `--dry-run` performs the full walk and sniff with zero writes and prints
   the would-change counters (bytes are plaintext bytes, computed via the
   encrypt layer's `Stat` for sealed files). Real runs print a progress
   line every 100 files plus a final one, then a summary
   (`scanned=/changed=/skipped=/failed=/bytes=`).

## Alternatives Considered

### Parallel workers
- Pros: faster on high-latency backends (s3).
- Cons: complicates failure/progress semantics, multiplies memory by the
  worker count, and races the "one writer per file" assumption for no
  correctness gain; an operator maintenance command values predictability
  over throughput. Rejected for v1; the walk is serial by design and could
  be parallelized later behind the same `Sweep` signature.

### Streaming copy instead of whole-file buffering
- Pros: constant memory regardless of file size.
- Cons: integrity (GCM) only verifies as chunks are read, so a wrong key or
  corrupt chunk surfaces mid-write; backends install on `Close`, so
  aborting then requires not closing — leaking temp objects on the failure
  path the sweep hits *by design* on every wrong-key file. Buffering first
  makes the read fully validated before any write starts and keeps the
  failure path clean (nothing written). Chosen; one file's plaintext in
  memory is acceptable for a maintenance tool.

### Maintenance-mode enforcement / a locking protocol
- Pros: eliminates the concurrent-write race entirely.
- Cons: ncgo has no maintenance-mode or storage-lock primitive; adding one
  for a rare operator command is disproportionate, and the race costs at
  most one file's concurrent edit (reported, re-runnable). Documented
  recommendation instead.

### Touching the filecache to record encoding state
- Pros: could memoize the sniff.
- Cons: the filecache tracks content metadata; encoding is a storage-layer
  fact the decorator already derives on read. Zero-touch keeps the sweep
  invisible to every other subsystem. Rejected.

## Consequences

- New CLI surface: `ncgo-cli encryption encrypt-all|decrypt-all
  [--user uid] [--dry-run]`; `cmd/ncgo-cli/deps.go` splits the raw-backend
  switch out of `openStorage` (`openRawBackend`) so the sweep can hold the
  raw backend and the encrypt wrapper separately. `openStorage` semantics
  for existing callers are unchanged; `app.openStorage` is untouched.
- The sweep rewrites stored bytes without changing plaintext content, so
  DAV/clients/filecache/versions observe nothing; stored sizes change by
  the header+tag overhead (~0.024%) in the seal direction.
- `encrypt-all` on a tree containing E2EE blobs seals them a second time at
  rest, which is harmless (ADR-0052 §7); `decrypt-all` restores the blob.
- A `decrypt-all` followed by disabling encryption completes
  decommissioning; the reverse order would strand sealed data (ADR-0052
  consequence), which the command help calls out implicitly by requiring
  the enabled flag.
- Open follow-ups from ADR-0052 remain: key rotation (the sweep is the
  re-encode engine a rotation flow would drive, but no key-ID header
  exists yet), per-user keys, filename encryption, SSE-C.

## Verification

- `internal/storage/encrypt/sweep_test.go`: mixed tree (plaintext, already
  sealed, nested dirs, empty file, sub-magic-length file) seals completely
  with byte-exact round trips and exact stats; idempotent re-seal skips
  everything; open direction is symmetric (seal → open restores exact
  plaintext, second open skips all); `--user`-style prefix scopes the walk
  (out-of-prefix files untouched, missing prefix is an empty sweep);
  dry-run counts correctly with zero writes in both directions; a wrong
  master key aborts the open sweep at the first sealed file with an
  `ErrIntegrity`-wrapping key-mismatch error (later files untouched);
  injected Open/Create faults count `Failed` and continue; a pre-cancelled
  context stops the walk; nil storages and unknown directions are rejected.
- `cmd/ncgo-cli/encryption_test.go`: end-to-end CLI flow over a localfs
  backend — `--user` scoping, dry-run counters with no writes, full
  encrypt-all with progress line and summary, idempotent re-run,
  decrypt-all restoring exact bytes, second decrypt-all skipping;
  `encryption.enabled: false` refuses both subcommands; missing key file
  fails closed; invalid `--user` values rejected.
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` no-op,
  `go test -race -coverprofile` green.
