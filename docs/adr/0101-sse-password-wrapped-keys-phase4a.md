# ADR-0101: Phase 5w-4a SSE password-wrapped user keys — enrollment + identity path

- **Status**: Accepted
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0100 (delivers its phase 4-a; records two implementation
  refinements — the session-copy key-ID byte and the FK-availability corner,
  both strike-annotated there)

## Context

ADR-0100 designed password-wrapped user keys and phased the implementation:
4-a is migration 0021, the crypto constructions, enrollment/unenrollment at
password login, the session key attach, the Resolve/Allocate identity path
with `ErrKeyLocked` → 403, reset-password refusal, sweep locked-skip, and
status counts. This ADR records 4-a **as built**, including the two places
the build refined the design and the interpretation calls the code had to
make. Phase 4-b (app-token wraps) remains out of scope, exactly as §7 of
ADR-0100 phases it.

Verified facts that shaped the build:

- **The login-v2 grant request carries the account password here.**
  ADR-0100 §4 speaks of "the authenticating session" attaching the key at
  grant time, but this server's `LoginV2.requireAuth` authenticates picker
  and grant requests with HTTP Basic over the verifier chain — the password
  is in hand, no session middleware is involved. The unlock therefore runs
  in `requireAuth` (a basic-authenticated grant request IS a password
  login) and `setSessionCookie` copies `principal.UnlockedKey` onto the
  issued session, which is the pinned shape ("no password here — the key
  comes from the existing session") with the session being the one the
  basic auth just verified.
- **`KeySharer.WrapForWrite`/`ReWrapForOverwrite` resolve the FK in the
  caller's ctx.** For an enrolled OWNER's fresh key that Resolve fails —
  the write runs as the owner but the ctx principal is the share writer —
  so the write path must thread the plaintext FK it holds from `Allocate`
  (see §5). Reordering the hooks cannot fix this: both need the FK.
- **Keyring rotation predates sessions.** ADR-0100 §3 sealed session
  copies under "ring[current]" with no key ID; a rotation would strand
  every live session's copy behind an unmarked ring position. The v2
  header and the UK row both already record key IDs — the session copy
  follows the same precedent (one byte).
- **`MintMissingUserKeys` would re-mint enrolled users' UKs.** Enrolled
  users have no `user_keys` row by construction; the ADR-0099 backfill
  selected exactly those users. The enrollment promise requires the
  exclusion.
- **files stays encrypt-free except one seam.** The WebDAV 403 mapping
  must match `encrypt.ErrKeyLocked` while `internal/webdav` stays
  encrypt-free; the dual-match wrapper lives in `internal/files`
  (keylocked.go), the package's single narrow encrypt import.

## Decision

### 1. Migration 0021 (three dialects in lockstep)

`user_key_pw(user_id PK, public_key, pw_sealed_uk, pw_kdf TEXT, pw_salt,
created_ms)`; `file_keys ADD COLUMN scheme INTEGER NOT NULL DEFAULT 0` (0 =
symmetric `NCGOFK1`, 1 = X25519 box); `sessions ADD COLUMN sealed_uk BLOB
NULL`. Column types mirror each dialect's 0020/0001 conventions. Down drops
all three (sqlite ≥3.35 `DROP COLUMN`, as 0020's down already uses).

### 2. Config

`encryption.password_wrapped_keys` (bool, default false) requires
`encryption.per_user_keys` at load time (same dead-key rule style as
per-user-keys-requires-enabled). Wiring sets `resolver.PasswordWrapped` and
`resolver.KDF` (from `auth.argon2id`) in `app.openStorage` and the CLI's
`perUserResolver`, so the server, sweeps, and user administration agree on
the deployment mode.

### 3. Crypto constructions (`internal/storage/encrypt/pwbox.go`)

Byte-exact per ADR-0100 §3: argon2id password KEK with stored
`m=…,t=…,p=…` parameters (strict marshal/parse; malformed → loud error);
`pw_sealed_uk` (60 B) via the shared `wrapSeal`/`wrapOpen` with
`pwAD(userID)`; X25519 keypair; the 92 B scheme-1 box (ephemeral ECDH,
HKDF-SHA256 info = AEAD AD = `"NCGOBX1" || keyUUID(16) || be64(user_id)`).
**Refinement one** (strike-annotated in ADR-0100): the session copy is
`keyID(1) || nonce(12) || AES-256-GCM(ring[keyID], priv, "NCGOSK1" ||
session-id)` — sealed under the current ring position, opened via the
recorded byte (bounds-checked), so a master-key rotation never strands live
sessions while the ring stays append-only. Session-copy, box, and
pw-seal authentication failures wrap `ErrIntegrity` where the caller-facing
semantic is wrong-key/corrupt — except the login password-unwrap failure,
which is a plain descriptive error (§4), and `ErrKeyLocked`, which is only
the read-path "no unlocked key available" sentinel.

### 4. Enrollment state machine (`resolver_pw.go`, concrete-only)

`UnlockForLogin(ctx, uid, password)` runs the four pinned branches
(enrolled/unenrolled × flag on/off), ordered so a crash re-runs the same
branch: unenroll re-mints/keeps the symmetric UK and converts every
scheme-1 row back before deleting `user_key_pw`; enrolled-with-UK is the
interrupted-enrollment resume (finish the box conversion, then delete the
UK); enroll inserts `user_key_pw` first (a concurrent-login unique
violation recurses into the enrolled branch) and deletes the UK last.
Password-unwrap AEAD failure is a descriptive "password unwrap failed —
password changed out-of-band?" error at login, never `ErrKeyLocked` and
never silent re-enrollment. A UK row that vanishes between the resume check
and the load means a concurrent login completed the conversion (only this
code deletes `user_keys`, after converting) — treated as done, which the
enroll-race test pins. `Enrolled`/`DestroyEnrollment` serve the CLI;
`SealSessionKey`/`UnsealSessionKey` are the session-copy API.
`OnUserDeleted` additionally purges `user_key_pw`; `MintMissingUserKeys`
excludes enrolled users.

### 5. Resolve/Allocate identity path

`Resolve`: owner with a `user_keys` row → the phase 1–3 master→UK→owner-row
path, unchanged. Enrolled owner → resolve through the READING user's wrap
row with `auth.UserFromContext`'s principal: unenrolled reader opens their
scheme-0 row via the master path (no key on the ctx needed); enrolled
reader box-opens their scheme-1 row with `principal.UnlockedKey`. No
principal, unknown reader, missing key, or missing row → `ErrKeyLocked`
(never `ErrIntegrity`/`ErrUnresolvableKey`); a box authentication failure
wraps `ErrIntegrity`. `Allocate` boxes an enrolled owner's FK with their
public key — no ctx, no session. `wrapFKForUser` is scheme-aware (enrolled
recipient → box, never lazy-mint; unenrolled → symmetric).

**Refinement two — the FK-availability corner** (strike-annotated in
ADR-0100 §5): `WrapKeyFor`/`ReWrapSharees` source the FK from `Resolve` in
the caller's ctx, which for an enrolled owner's key succeeds only when the
ctx carries an authorized reader. The write path therefore threads the FK
it holds from `Allocate`: the encrypt FS's v3 writer exposes it
(`storage.FileKeyWriter.PlainFileKey`), `dav.write` captures it alongside
the key UUID, and `KeySharer.WrapForWrite`/`ReWrapForOverwrite` pass it to
the resolver's `WrapKeyForFK`/`ReWrapShareesFK` through the
`FKThreadingWrapper` seam. Hooks firing in a third-party ctx (an admin's
group-member-add) still fail best-effort for enrolled-owner files —
existing hook semantics: logged, never fatal — and heal on the next
authorized write. **Residual gap**: an enrolled member newly added to a
group share owned by an enrolled user gains access at the next write by any
authorized party (or a re-share). The plaintext FK's transit through the
DAV layer adds no exposure — that layer already handles plaintext content;
the keyed writer hands out copies and never logs it.

### 6. Session + principal plumbing

`session.Session` gains `SealedUK` (nullable column scan); `session.Store`
gains `SetSealedUK`. `auth.Principal` gains `UnlockedKey` (never logged or
serialized). `auth.SessionVerifier` gains the structural
`SessionKeyUnlocker` seam (`*encrypt.SQLResolver` satisfies it; auth does
not import encrypt): a session row carrying `sealed_uk` unseals onto the
principal, and an unseal failure rejects the session
(`ErrInvalidCredentials`, fail-closed — a corrupt copy must not silently
degrade to per-file 403s). The middleware best-effort-zeroes an attached
key after the request. `web.BrowserLogin`/`web.LoginV2` gain the structural
`LoginKeyHandler` seam: basic logins (and basic-authenticated grant
requests, §Context) run `UnlockForLogin` — app-password auth never does —
and the issued session stores the sealed copy; a seal/store error deletes
the session and 500s, because a session without its key copy would 403
every file. App routes widen the resolver to these interfaces only when
non-nil (the typed-nil interface trap, same guard as the FS wiring).

### 7. CLI

`user reset-password` refuses enrolled users without `--force` (pinned
message naming the PERMANENTLY UNREADABLE consequence and the remedy);
`--force` runs `DestroyEnrollment` before the hash update and prints a loud
DATA LOSS line. `encryption status` prints `password-wrapped keys: <flag>`
and `password-wrapped users: N`, and the wraps line gains the box count.
Sweep summaries print `locked-skipped=N` when nonzero; a locked skip is
never a failure (`SweepStats.LockedSkipped`, `OnError` not called). Only
decrypt-all meaningfully hits it — encrypt-all skips sealed files,
rotate-keys/rekey-v3 skip v3 by rule, and v3 writes box via public keys.

### 8. WebDAV 403 mapping

`internal/files/keylocked.go`: `keyLockedError` dual-matches
`webdav.ErrForbidden` (the handler's existing 403 path) and
`encrypt.ErrKeyLocked` (tooling never reports a lock as corruption);
`Unwrap` preserves the chain. Threaded at `mapStorage`'s default (every
storage-boundary Open/Create in dav.go — reads, writes, public-link
downloads via `PublicDAV` → `dav.Read`) and at the version-snapshot read in
the write path. `internal/webdav` stays encrypt-free.

### Alternatives considered

- **Reordering the write-path hooks instead of threading the FK** —
  `WrapForWrite` before `ReWrapForOverwrite` still resolves the FK through
  the writer's ctx, which an enrolled writer cannot satisfy for the fresh
  owner-boxed key; the chicken-and-egg is inherent. Threading is the
  minimal correct change. Rejected.
- **Boxing the FK for the ctx principal at Allocate** — Allocate could
  wrap for an enrolled share-writer's public key at write time, but that
  special-cases one hook's need while grant/group hooks keep failing on the
  same corner; the threaded FK fixes the whole class. Rejected.
- **Session copies without a key-ID byte** (ADR-0100 §3 as written) — a
  rotation strands live sessions' copies or forces a scan; the recorded
  byte follows the v2-header and UK-row precedent for one byte. Refined
  (§3).
- **Best-effort login** (unlock failure → proceed keyless) — silently
  degrades an enrolled user's every file to 403 and masks account
  inconsistency; loud 500s are the design's fail-closed posture. Rejected.

## Consequences

- **The threat-model pivot is live.** After enrollment deletes the UK, an
  at-rest compromise (database + master key + config) exposes no file
  content for enrolled users without an active session. `Resolve`'s read of
  an enrolled owner's file now depends on the request principal — the first
  read path in the server that requires an authenticated identity beyond
  share permissions.
- **Phase 4-a breaks app-password DAV access for enrolled users** (no
  token wraps until 4-b), as the ADR-0100 trade-off table states; public
  links and background readers of enrolled files 403 the same way. Both are
  opt-in, documented limitations.
- **Rollback**: down-migration drops all of 0021; enrolled users must
  unenroll FIRST (flag off + password login), else their rows' meaning is
  lost — ADR-0100 §8's rule, extending the v2/v3 rollback rule verbatim.
  Binary rollback to a pre-4-a build strands enrolled users (their
  `user_keys` rows are gone).
- **Mixed population is the steady state**: unenrolled users behave
  exactly as in phases 1–3, and cross-population sharing works
  per-recipient — an unenrolled reader opens an enrolled owner's file via
  their own scheme-0 row; an enrolled reader needs their session key.
- **Transient crash-state reads**: mid-enrollment (both key rows present,
  wraps half-converted) a read may fail `ErrUnresolvableKey` until the next
  password login completes the resume branch; accepted, since the resume is
  pinned and idempotent.
- **`KeyResolver` is untouched** (concrete-only additions);
  `session.Store` gains `SetSealedUK`; `files.KeySharer.WrapForWrite` /
  `ReWrapForOverwrite` gain an FK parameter (nil = Resolve as before).
  Zero new third-party dependencies (`golang.org/x/crypto` was already in
  go.mod; hkdf is used from x/crypto for uniformity).

## Verification

- `pwbox_test.go`: pw_sealed_uk round-trip (60 B; wrong password/user/AD,
  tamper, wrong params all fail); keypair shape; box round-trip (92 B,
  ephemeral randomness differs per wrap; wrong keyUUID/userID AD, tamper,
  wrong recipient key, truncation all fail); `pw_kdf` marshal/parse incl.
  malformed rejections; session copy round-trip incl. ring extension (seal
  under key 0 of a 1-key ring, unseal with the 2-key ring via the recorded
  byte; new copies seal at the current position; wrong session/tamper/
  out-of-ring ID → `ErrIntegrity`).
- `resolver_pw_test.go`: enrollment at login (UK deleted, rows scheme=1,
  same FKs via identity ctx, `ErrKeyLocked` without; re-login stable; wrong
  password loud + state untouched); unenrollment (nil return, UK restored,
  scheme=0, master path); crash-resume BOTH directions (fabricated
  both-rows mixed-scheme state, each flag value); `Enrolled`/
  `DestroyEnrollment` (other users' rows untouched, unknown uid no-op);
  the Resolve matrix (enrolled owner × enrolled/unenrolled/keyless/
  non-recipient/unknown readers — lock cases are `ErrKeyLocked`, never
  `ErrIntegrity`/`ErrUnresolvableKey`); Allocate for an enrolled owner with
  a zero-value ctx (scheme=1 row; FS-level Create/Open round-trip and
  principal-less lock); concurrent-enrollment race (8 workers, one row, one
  keypair) and Resolve-vs-enroll race (legal outcomes only) under `-race`.
- `internal/session`: SealedUK round-trip, SetSealedUK, nil default,
  unknown session `ErrNotFound`.
- `internal/auth`: key attach on sealed_uk present; corrupt copy →
  `ErrInvalidCredentials` (fail-closed); nil Keys / nil copy attach
  nothing; middleware zeroes the attached key after the request.
- `internal/web`: basic login stores sealed_uk on the session row;
  app-password login never calls the key handler (spy) and stores nothing;
  unlock error → 500 with no session; store error → 500 + session deleted;
  login-v2 grant unlocks and copies, store error → 500 + deleted, nil Keys
  inert.
- `internal/files`: enrolled owner + enrolled recipient incoming overwrite
  carries wraps (old rows deleted, both parties read, keyless read
  `ErrKeyLocked`) — the FK-threading pin; HTTP GET as app-password
  principal → 403, with unlocked session → 200; public-link download of an
  enrolled file → dual-matched 403/`ErrKeyLocked`; unenrolled link control
  reads fine; keyshares unit tests keep green with the FK parameter.
- `cmd/ncgo-cli`: reset-password refusal without `--force` (state
  untouched) and DATA-LOSS `--force` (enrollment + wraps destroyed,
  password reset, destroyed file unresolvable); status prints
  password-wrapped counts; decrypt-all over an enrolled tree exits 0 with
  `locked-skipped=2`, files still v3-sealed.
- `internal/migrations`: 0021 up/down/up in lockstep (user_key_pw, scheme,
  sealed_uk present at v21, gone at v20, 0020 objects intact).
- `internal/config`: password_wrapped_keys default/off, override parse,
  requires-per_user_keys validation.
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` no-op (zero new
  dependencies), `go test -race ./...` green.
