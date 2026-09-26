# ADR-0100: Phase 5w-4 password-wrapped user keys — design

- **Status**: Accepted (design; implementation phased as §8)
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0096 (supplies the phase-4 design it deferred to "that
  phase's ADR"; revisits its symmetric-only key decision under the phase-4
  evidence below)

## Context

ADR-0096 phased the per-user key hierarchy and pinned phase 4 only as a
sketch: *"UK additionally sealed under a password-derived KEK (argon2id);
unlocked into the session at password login; app-password/token sessions
carry no password and cannot unlock. This constraint and the trade-off
table belong to that phase's ADR."* Phases 1–3 have since landed
(ADR-0097 envelope + resolver, ADR-0098 share wrap/revoke, ADR-0099
lifecycle + tooling). This ADR is phase 4's own ADR: the threat-model
change, the full design, and the trade-off table.

Verified facts that shape the design:

- **The threat model that changes** (ADR-0096): after phase 4, a
  server-at-rest compromise (database + master key file + config) exposes
  no file content for users **without an active unlocked session**.
  Phases 1–3 keep every UK openable by the server-held master key; phase 4
  must therefore REMOVE the master-sealed UK of an enrolled user, or the
  promise is void.
- **argon2id is not a new dependency.** ADR-0096 budgeted it as the
  repo's first new dependency, but `internal/auth` already hashes
  passwords with `golang.org/x/crypto/argon2` (go.mod: `golang.org/x/crypto`
  v0.57.0). curve25519 and hkdf ship in the same module. Phase 4 adds
  **zero** third-party dependencies.
- **App passwords authenticate without the login password**
  (`internal/auth.AppPasswordVerifier`: only token hashes are stored).
  Browser login (`internal/web.BrowserLogin`) runs a ChainVerifier that
  accepts app passwords too, but the returned `Principal.AuthMethod`
  distinguishes them (`basic` = real password).
- **Sessions are server-side rows** (`internal/session`, migration 0001)
  with no payload column today; `SessionVerifier.VerifyID` resolves the
  cookie to a Principal on every request — the natural place to reattach
  an unlocked key.
- **No self-service change-password route exists.** The only password
  mutation is admin CLI `ncgo-cli user reset-password`.
- **The lock problem breaks ADR-0096's symmetric-only decision.** With a
  purely symmetric UK, wrapping a file key FOR a user requires their UK —
  but an enrolled user's UK must not be server-held. Three existing flows
  wrap for users who are not the requester: share grant to an offline
  user (`KeySharer.WrapForShare`, run in the GRANTER's request),
  incoming-share writes remapped to the owner's path (`Allocate` derives
  the owner from the storage key; the owner's UK is locked), and group
  membership add. ADR-0096 rejected asymmetric keys *"for now … Revisit
  if an end-to-end design ever lands"* — phase 4 is that revisit, and the
  justification is server-side: **wrapping for a locked user is
  impossible with symmetric keys alone.**

## Decision

### 1. Enrollment model

Deployment-level opt-in: `encryption.password_wrapped_keys: true`
(default false; requires `encryption.per_user_keys: true`, else config
load fails loudly). Per-user enrollment is **lazy at the user's first
password login** after the flag turns on — the only moment the server
holds the password. A mixed enrolled/unenrolled population is the normal
steady state; unenrolled users behave exactly as in phases 1–3. Nobody
can enroll another user (CLI cannot mint a password wrap it doesn't
know). Setting the flag back off stops new enrollments, and each
enrolled user **unenrolls at their next password login** (their key is
unlocked at that moment, so the symmetric UK can be re-minted and every
wrap row converted back — see §5).

### 2. Enrolled key material

The enrolled user's UK **is an X25519 keypair**:

- `public_key` (32 B): server-held plaintext — it is public. Any flow
  can wrap FOR the user without their session (the lock problem's fix).
- private key (32 B): never server-held in plaintext. At rest it exists
  only as `pw_sealed_uk` (§3); in flight it lives in session rows
  (master-sealed, §4) and request contexts.

Enrollment is one restartable pass at password login:

1. Generate the keypair; seal the private key under the password KEK;
   insert the `user_key_pw` row.
2. Re-wrap **every existing `file_keys` row of the user** (the ADR-0098
   recipient rows make them enumerable by `user_id`): open with the old
   symmetric UK (in hand), re-seal as a box (§3), set `scheme = 1`. Rows
   already at `scheme = 1` are skipped, so an interrupted enrollment
   simply re-runs.
3. Only when no `scheme = 0` row remains for the user: **DELETE the
   `user_keys` row** — the master key no longer opens anything of
   theirs. This is the threat-model pivot.

Unenrollment (flag off + password login) is the mirror: re-mint the
symmetric UK, open each box row with the unlocked private key, re-wrap
symmetric (`scheme = 0`), delete the `user_key_pw` row.

### 3. Crypto constructions (pinned byte-exactly)

- **Password KEK**: `KEK = argon2id(password, pw_salt, params)` → 32 B,
  `pw_salt` 16 random bytes; params come from `auth.argon2id` config and
  are stored PHC-style in `pw_kdf` (`m=…,t=…,p=…`) so later KDF-param
  changes never strand rows.
- **pw_sealed_uk** (60 B): `nonce(12) || AES-256-GCM(KEK, privkey(32),
  "NCGOPW1" || be64(user_id))`.
- **FK box wrap, `scheme = 1`** (92 B): ephemeral X25519 keypair;
  `shared = X25519(eph_priv, recipient_pub)`;
  `wrapKey = HKDF-SHA256(shared, salt=nil,
  info="NCGOBX1" || keyUUID(16) || be64(user_id))`;
  blob = `eph_pub(32) || nonce(12) || AES-256-GCM(wrapKey, FK,
  "NCGOBX1" || keyUUID(16) || be64(user_id))`. The HKDF info and AEAD AD
  bind file + recipient exactly as the symmetric scheme (`NCGOFK1`), so a
  wrap replayed onto a different file or user fails authentication.
  Ephemeral ECDH needs no sender authentication — wraps are created
  exclusively server-side.
- **Session copy** (`sessions.sealed_uk`): ~~`nonce(12) ||
  AES-256-GCM(ring[current], privkey, "NCGOSK1" || session_id)`~~
  (**refined by ADR-0101**: the blob gains a key-ID byte — `keyID(1) ||
  nonce(12) || AES-256-GCM(ring[keyID], privkey, …)` — for O(1) ring
  selection after master-key rotation). The
  master-sealed copy is the session's only key material; logout, expiry,
  and session deletion destroy it. At-rest exposure is exactly "users
  with an active session" — the stated threat model.
- **Token wrap** (phase 4-b, §7): `KEK = HKDF-SHA256(token, salt(16
  random), "NCGOAK1" || be64(user_id))` (app-password tokens are ≥256-bit
  random — stretching is unnecessary, and argon2id per request would be
  a self-DoS vector); blob = `nonce(12) || AES-256-GCM(KEK, privkey,
  "NCGOAK1" || be64(user_id))`.
- Symmetric `scheme = 0` wraps (`NCGOFK1`, ADR-0097) are untouched.

### 4. Session and request plumbing

- At password login (`AuthMethod == basic` only — an app password
  presented at the browser form never enrolls or unlocks): enroll if
  due, else unwrap `pw_sealed_uk` with the password KEK. AEAD failure
  after a successful password-hash verify means account inconsistency
  (out-of-band password change): fail the login loudly, never fall back
  to silent re-enrollment (which would orphan every file).
- The unlocked private key is stored master-sealed on the session row;
  `SessionVerifier.VerifyID` unseals it (app wires the keyring into the
  verifier) and attaches it to the `Principal` (`UnlockedKey []byte`,
  nil for app-password/bearer/anonymous requests in phase 4-a). The
  field is never logged; principals are never `%v`-dumped.
- The unlocked key lives in request-scope memory; best-effort zeroing on
  request end (Go gives no guarantees — noted, accepted).

### 5. Read and write paths

- **Resolve (read)**: owner unenrolled → today's master→UK→FK path,
  unchanged. Owner enrolled → resolve through the **reading user's own
  wrap row** — the ADR-0098 recipient rows become the read path, their
  pinned phase-4 purpose. The reader's identity and key come from the
  request context: enrolled reader → box-open their `scheme = 1` row
  with the session key; unenrolled reader → their `scheme = 0` row via
  the master path (mixed populations interoperate per-recipient). No
  principal, no unlocked key, or no row for the reader → new sentinel
  **`ErrKeyLocked`** (strictly distinct from `ErrIntegrity` and
  `ErrUnresolvableKey`): WebDAV maps it to 403 with an explicit
  "encrypted: key locked" message; operator tooling must never report it
  as corruption.
- **Allocate (write)**: wraps the fresh FK for the storage-key owner —
  unenrolled: symmetric as today; enrolled: box with their public key,
  no session needed. The writer (when not the owner, i.e. an incoming
  share write) and covering-share recipients are wrapped by the existing
  ADR-0098 hooks, per-recipient by scheme, ~~again without any session
  (public keys)~~ (**refined by ADR-0101**: the recipient-side wrap truly
  needs no session — boxes are public-key — but the FK SOURCE does for an
  enrolled owner's key: `Resolve` succeeds only in an authorized reader's
  ctx, so the write path threads the FK from `Allocate`, and hooks firing
  in a third-party ctx fail best-effort and heal on the next authorized
  write). `WrapKeyFor` for an enrolled uid never lazy-mints — the
  keypair exists by construction.
- **Revoke**: row deletion, unchanged — with enrolled recipients it
  becomes cryptographically real (ADR-0096's phase-4 promise).
- **Public links and anonymous reads** of enrolled owners' v3 files:
  `ErrKeyLocked` (no session, no key). Accepted limitation; link shares
  keep working for unenrolled users. Revisit per-link wraps separately.
- **Server-side background readers** (jobs, preview pre-generation, any
  content read without a request context) of enrolled users' files fail
  the same way. Previews generated during live sessions are unaffected;
  the `appdata_` system tree stays master-sealed v1/v2 (the ADR-0097
  leftover, unchanged by this ADR).
- **CLI sweeps** (`encrypt-all`/`decrypt-all`/`rotate-keys`/`rekey-v3`)
  run without principals: files whose FK cannot be resolved are SKIPPED
  as "locked" with a summary count (v3 files need no rekey; `rotate-keys`
  only retires v2 key IDs; `decrypt-all` cannot decrypt enrolled users'
  files — that is the point of enrollment). A locked skip is never a
  sweep failure.
- **Master-key rotation**: enrolled users hold no master-sealed
  material; `ResealUserKeys` skips them by construction (their
  `user_keys` rows are gone). Session copies ride the append-only ring
  and expire naturally.

### 6. Password lifecycle

- `ncgo-cli user reset-password` on an enrolled user **refuses** without
  `--force`: the old password is unknown, so the private key cannot be
  re-wrapped. `--force` deletes the `user_key_pw` row and ALL the user's
  `file_keys` rows (their v3 files are permanently unreadable), prints
  an explicit data-loss warning, and the user's next password login
  enrolls them fresh (flag on) or returns them to master-wrapped (flag
  off).
- When a self-service change-password route lands, it MUST re-seal the
  private key under the new password (old password verifies first). The
  public key is unchanged, so no `file_keys` row moves — password change
  is O(1).

### 7. App-password and token sessions (the ADR-0096 constraint)

- **Phase 4-a** (first implementation increment): app-password/bearer
  requests carry no key; v3 I/O for enrolled owners fails with
  `ErrKeyLocked` → loud 403. Guidance: users who need DAV clients stay
  unenrolled, or the deployment stays master-wrapped (ADR-0096's
  recommendation stands).
- **Phase 4-b** (follow-up increment, this ADR): per-token key wraps.
  App passwords are issued inside an unlocked browser session (login-v2
  grant) with the raw token in hand: seal the private key under the
  token KEK (§3) into `app_token_keys`. The app-password verifier unwraps
  per request (bounded in-memory KEK cache keyed by token hash) and
  attaches the key to the Principal — DAV clients keep working for
  enrolled users. Revoking the token deletes its wrap row: cryptographic
  revocation. Tokens issued BEFORE enrollment hold no wrap; those
  clients fail loudly until the token is re-issued (documented).

### 8. Schema (migration 0021, three dialects in lockstep) and phases

- `user_key_pw(user_id PK, public_key BLOB NOT NULL, pw_sealed_uk BLOB
  NOT NULL, pw_kdf TEXT NOT NULL, pw_salt BLOB NOT NULL, created_ms INT
  NOT NULL)`.
- `file_keys ADD COLUMN scheme INTEGER NOT NULL DEFAULT 0` (0 =
  symmetric `NCGOFK1` wrap; 1 = X25519 box). The per-row scheme makes
  enrollment re-wraps resumable and mixed reads unambiguous.
- `sessions ADD COLUMN sealed_uk BLOB NULL`.
- Phase 4-b adds `app_token_keys(app_password_id TEXT PK, sealed_uk
  BLOB NOT NULL, salt BLOB NOT NULL, created_ms INT NOT NULL)`.
- Down-migration drops all of the above; enrolled users must unenroll
  FIRST (flag off + password login), else their rows' meaning is lost.
  Binary rollback to a pre-phase-4 build strands enrolled users' file
  access (their `user_keys` rows are gone) — the documented v2/v3
  rollback rule extends verbatim.

Implementation phases (each its own increment, standard gates):

- **4-a**: migration 0021; box + password-KEK constructions with pinned
  test vectors; enrollment/unenrollment at password login; session key
  attach + middleware plumbing; Resolve/Allocate identity path with
  `ErrKeyLocked` → 403; reset-password refusal/`--force`; sweep
  locked-skip; `encryption status` enrollment counts; docs.
- **4-b**: `app_token_keys`, wrap-at-issuance, verifier unlock + KEK
  cache, revocation.
- **Later, not this ADR**: public-link access for enrolled users
  (per-link wraps), recovery export tooling, self-service
  change-password route (its re-wrap duty is pinned in §6).

### Alternatives considered

- **Loud-fail writes into enrolled users' shares** (symmetric-only):
  incoming-share writes and grants to offline users would fail — a
  silent collaboration regression discovered per deployment. Rejected.
- **Server-held pending-wrap queue** (symmetric-only): deferred wraps
  would store FKs master-sealed until the recipient's next login — the
  server can read those files at rest, quietly voiding the enrollment
  promise for exactly the files collaboration produces. Rejected as
  dishonest; loud failure was preferable, and both lose to keypairs.
- **Replace symmetric UKs for everyone**: forces the threat model (and
  its client constraints) on deployments that don't want it. Master-key
  mode stays the recommended default (ADR-0096, upstream precedent);
  per-user mixed population is required. Rejected.
- **Signed wraps (sender-authenticated ECDH)**: wraps are minted only by
  the server; sender authentication buys nothing against a malicious
  server (which can re-wrap regardless). Ephemeral ECDH + AEAD integrity
  is sufficient. Rejected.

## Consequences

- Phase 4 is the first phase that changes the security posture: an
  at-rest server compromise exposes no file content for enrolled users
  without an active session. For unenrolled users (and for link shares,
  background jobs, and CLI sweeps touching enrolled users' files)
  nothing improves — and phase 4-a actively breaks app-password DAV
  access for enrolled users until 4-b lands. Both are documented,
  opt-in, and per-user lazy.
- New config: `encryption.password_wrapped_keys` (bool, default false,
  requires `per_user_keys`). New sentinel `ErrKeyLocked`. New migration
  0021 (three dialects). Zero new third-party dependencies.
- Admin recovery (ADR-0096 §5) ends at enrollment: the master key no
  longer opens an enrolled user's chain. `reset-password` becomes a
  data-loss operation for enrolled users unless a live session or the
  password is available; the CLI refuses by default.
- ADR-0096's asymmetric-keys rejection is revisited and narrowed:
  symmetric UKs remain the design for master-wrapped mode; X25519
  keypairs exist solely to wrap for locked users in password-wrapped
  mode.
- The trade-off table:

  | Capability | Master-wrapped (phases 1–3) | Password-wrapped, enrolled |
  |---|---|---|
  | At-rest server compromise exposes files | yes | no (no active session) |
  | Admin/master-key recovery | yes | no |
  | Browser (password login) access | yes | yes |
  | App-password DAV clients | yes | 4-a: no; 4-b: yes (re-issue tokens) |
  | Public-link downloads | yes | no (v3 files) |
  | Background previews/jobs reading content | yes | no (live-session work unaffected) |
  | CLI sweep re-encode of their files | yes | skipped as locked |
  | Password reset without old password | harmless | data loss (`--force`) |
  | Revocation strength | policy-level | cryptographic |

## Verification (design level)

- 4-a pins: box/pw-KEK round-trip vectors (byte-exact constructions
  above), enrollment at password login (happy path, interrupted re-run
  idempotency, app-password login never enrolls), unenrollment on flag
  off, Resolve per-scheme through the reader's row (enrolled reader,
  unenrolled reader, owner, no-principal → `ErrKeyLocked`, wrong-key →
  `ErrIntegrity` never conflated), Allocate for a locked owner via
  public key, incoming-share write by a recipient into an enrolled
  owner's tree, reset-password refusal + `--force` semantics, sweep
  locked-skip accounting, middleware key attach (session copy
  round-trip; app-password/bearer → nil key), rollback rules.
- 4-b pins: wrap-at-issuance, per-request unlock through the verifier,
  KEK cache bounds, revocation deleting the wrap, pre-enrollment tokens
  failing loudly.
