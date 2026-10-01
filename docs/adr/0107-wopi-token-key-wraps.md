# ADR-0107: WOPI token-bound key wraps

- **Status**: Accepted
- **Date**: 2026-10-02
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0106 (delivers its token-bound key-wrap follow-up)

## Context

ADR-0106's WOPI host core left one documented gap: the mint and viewer
endpoints run session-authed (an enrolled ADR-0100 password-wrapped user's
unlocked key is on the principal), but the Collabora-facing callbacks run
**anonymous** — the `access_token` query parameter is the only credential —
so an enrolled user's callbacks hit `ErrKeyLocked` → 403 on every
operation. ADR-0102 solved the same shape of problem for app-password
tokens with per-token key wraps: seal the user's unlocked X25519 private
key under a KEK derived from the raw token at issuance, open it per
request, and let token deletion revoke the wrap cryptographically. This
ADR mirrors that construction for WOPI tokens, closing the office epic's
403 boundary for enrolled users.

Verified facts that shaped the build:

- **WOPI tokens are stored plaintext** (ADR-0106 §Context: machine-generated
  ≥256-bit bearer credentials, the login_flows/public-share precedent). The
  wrap rows therefore key off the token itself — none of ADR-0102's
  stored-hash indirection (primary/legacy hash dance) is needed.
- **A WOPI token is file-scoped.** ADR-0102's app-token AD binds the wrap to
  the owning user only; here the associated data must also bind the file id,
  or a wrap sealed for one document could be replayed against another of
  the same user's tokens' files.
- **The mint path holds both secrets.** `Service.Mint` runs in the caller's
  session ctx (both `MintHandler` and `ViewerHandler` call it), so the raw
  token and the principal's unlocked key meet at exactly one point — the
  same wrap-at-issuance moment ADR-0102 §3 pins for login-v2 grants.
- **The WOPI GC already owns token expiry.** `wopi.tokens.gc` sweeps
  expired rows; with wraps keyed by the token, the sweep is also the
  revocation path, mirroring ADR-0102 §5's hook-plus-prune coverage.

## Decision

### 1. Migration 0026 `wopi_token_keys` (three dialects in lockstep)

`token <PK>, sealed_uk <blob>, salt <blob>, created_ms <int64 ms>` — column
types keyed off each dialect's `wopi_tokens.token` (sqlite/postgres `TEXT`,
mysql `VARCHAR(255)`) and 0022's blob conventions. No FK clause, matching
the 0020/0021/0022 explicit-lifecycle convention: the GC sweep (§5) owns
cleanup. Down drops the table.

### 2. Crypto construction (`internal/storage/encrypt/resolver_wopitoken.go`)

Byte-level sibling of ADR-0102 §2: `KEK = HKDF-SHA256(tokenRaw, salt(16
random), "NCGOWK1" || be64(user_id) || be64(file_id))`; blob =
`nonce(12) || AES-256-GCM(KEK, privkey(32), same AD)` = 60 B, via the
shared `wrapSeal`/`wrapOpen` and `hkdfSHA256Salt`. **New AD domain
`NCGOWK1`** — never `NCGOAK1`: the AD is the only format binding between
the two wrap families, and the WOPI AD adds the file id because the token
is file-scoped.

`WrapKeyForWOPIToken(ctx, tokenRaw, fileID, priv)` resolves the owning user
through `wopi_tokens ⋈ users ON uid` (unknown token → error; `priv` must be
32 bytes), seals with a fresh salt, and inserts; a unique violation is a
no-op (mint retries are safe). Error strings name the failure, never the
token — the token is the bearer credential itself. `UnlockForWOPIToken(ctx,
tokenRaw, fileID)` finds the wrap through the plaintext token
(`wopi_token_keys ⋈ wopi_tokens ⋈ users`); no row → `nil, nil` (pre-0026 or
keylessly-minted token — the callback attaches no key); an open failure
wraps `ErrIntegrity` (fail-closed, mirroring the app-token semantics: a
corrupt wrap must not silently degrade to per-file 403s).

### 3. Wrap-at-mint (`wopi.Service.Mint`)

After the token row inserts, when `Service.Keys` is wired (new narrow
`TokenKeyWrapper` seam, satisfied structurally by `*encrypt.SQLResolver`)
and the session principal carries a 32-byte `UnlockedKey`, the key is
sealed under the new token. **A wrap error fails the mint loudly** (500)
and rolls the token row back via `Store.Delete` — mirroring login_v2.go's
grant: a silently unwrapped token would 403 every callback. With no Keys
(encryption off) or no usable unlocked key (app-password-auth minter,
unenrolled user) the wrap is skipped silently — ADR-0102 §3's pinned
no-wrap behavior: such tokens authenticate but their callbacks keep the
403 boundary.

### 4. Callback unlock (`wopi.FilesHandler`)

The files handler mounts with no session middleware, so after the existing
authenticate-and-file-id-binding check it opens the token's wrap with the
exact `(token, fileID)` pair; a non-nil key is attached to the request
principal (`defer clear(priv)`, mirroring the auth middleware's key
hygiene) and the callback then resolves the file through the minter's own
file_keys wraps exactly like a session request. A nil return keeps the
anonymous ctx (pre-0026 and keyless-minter tokens keep the documented 403
boundary). An error maps through `mapError` (`ErrIntegrity` → 500,
fail-closed) — NEVER a silent keyless 403.

### 5. Bulk-GC revocation + orphan purge

The GC job gains a nil-tolerant `TokenKeyCleaner` seam
(`DeleteWOPITokenKeys(ctx, []string)`, `PruneWOPITokenKeys(ctx)`) and a
nil-tolerant logger, mirroring sharing's expire job. Sweep order: list the
expired tokens (`Store.ExpiredTokens`) → delete exactly their wraps →
delete the expired token rows → prune orphans (wraps whose token row is
gone, the safety net mirroring `PruneStaleKeys`). Key-side errors are
Warn-logged and never fail the row deletion. Revocation is cryptographic:
without the wrap row, a captured token opens nothing — and with the token
row gone too, the bearer itself stops verifying.

### 6. No key cache

Mirroring ADR-0102 §4: HKDF derive + AES-GCM open costs microseconds per
callback and the wrap lookup is one indexed PK join; a long-lived
key-material cache would add exposure surface for no measurable win.

### Alternatives considered

- **Reuse the `NCGOAK1` AD domain** — rejected: the AD is the only format
  binding; sharing it would make an app-token wrap and a WOPI-token wrap
  interchangeable across families (same user id space), and the WOPI wrap
  needs the file id bound, which the app-token AD shape lacks.
- **Hash WOPI tokens at rest and key wraps off the hash** — rejected:
  breaks ADR-0106's plaintext bearer precedent and buys nothing — an
  attacker reading the DB already holds the files the tokens protect, and
  the hash dance exists in ADR-0102 only because app passwords ARE hashed
  (user-typed-adjacent credentials).
- **Attach the wrap columns onto `wopi_tokens` itself** — rejected: a
  separate table mirrors 0022 exactly, keeps the 0025 rows narrow (the
  callback auth path touches no key material), and lets the wrap lifecycle
  (GC delete + orphan prune) run without widening the token store.
- **Fail-open callback unlock** (wrap error → proceed anonymous) —
  rejected for ADR-0102 §4's reason: it silently degrades an enrolled
  user's every callback to 403 and masks wrap corruption.

## Consequences

- **Enrolled password-wrapped users' WOPI callbacks now work end to end** —
  mint seals the session key under the token, and the anonymous callback
  resolves the file through the user's own `file_keys` wraps exactly like
  a session request. Per-callback cost is one indexed PK-join + HKDF +
  AES-GCM open (µs).
- **Pre-0026 tokens and keylessly-minted tokens keep the documented
  ErrKeyLocked → 403 boundary**; re-minting from an unlocked session
  (reload the viewer) is the remedy, same as ADR-0102's re-issue remedy.
- **The token TTL still bounds exposure**: a minted token outlives a
  permission change until expiry; the per-request permission re-check
  hardening remains an open follow-up (ADR-0106 Consequences), unchanged.
- **Rollback**: down-migration drops `wopi_token_keys`; enrolled users'
  callbacks return to the ADR-0106 403 boundary. Nothing else moves.
- Zero new third-party dependencies (HKDF/AEAD helpers already in-tree).

## Verification

- `internal/migrations`: 0026 up/down/up in lockstep (wopi_token_keys
  present at v26, gone at v25, wopi_tokens intact).
- `resolver_wopitoken_test.go`: wrap round-trip (60 B blob, 16 B salt, same
  key back); unknown token → nil, nil (the token IS the row key — no hash
  indirection); re-keyed row (KEK mismatch) and tamper → `ErrIntegrity`;
  file-id binding (wrap for file 7 does not open for file 8); unique
  re-wrap no-op (first wrap survives); batch + empty `DeleteWOPITokenKeys`;
  `PruneWOPITokenKeys` removes orphans only.
- `internal/wopi`: mint with a 32-byte session key wraps the exact
  (token, fileID, priv) triple; wrap failure → 500 AND the token row is
  gone; keyless/nil-Keys/short-key mints skip the wrap silently; callback
  unlock calls the exact (token, fileID) pair and proceeds 200, wrap error
  → 500 fail-closed, nil priv → anonymous flow unchanged; GC deletes
  exactly the expired tokens' wraps, prunes orphans, keeps live rows, and
  key-side errors never fail the sweep; store Delete/ExpiredTokens units.
- `internal/app`: office + per-user-keys encryption → `wopiSvc.Keys`
  non-nil; office without encryption → nil (widened-only-when-non-nil
  idiom).

## References

- ADR-0102 (app-token key wraps — the mirrored construction), ADR-0106
  (WOPI host core — the follow-up this lands), ADR-0101 (the ErrKeyLocked
  boundary), ADR-0100 (password-wrapped user keys).
