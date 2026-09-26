# ADR-0102: Phase 5w-4b SSE app-token key wraps

- **Status**: Accepted
- **Date**: 2026-09-26
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0100 (delivers its phase 4-b; records two refinements —
  the KEK-cache omission, strike-annotated in §7 there, and the
  `AppPasswordIssuer.Issue` signature widening)

## Context

ADR-0100 phased password-wrapped user keys into 4-a (enrollment + identity
path, built in ADR-0101) and **4-b**: per-token wraps of the enrolled user's
X25519 private key, so app-password/bearer clients of enrolled users unlock
files again (4-a leaves them at per-file `ErrKeyLocked` → 403). This ADR
records 4-b **as built**, including the places the build refined or had to
interpret the design.

Verified facts that shaped the build:

- **App passwords authenticate through two verifiers.** Nextcloud clients
  present the token either as the basic-auth password
  (`AppPasswordVerifier`, in the chain) or as a bearer token
  (`BearerVerifier`, with a 60-second uid cache). Both must attach the key;
  neither may import `encrypt` — the structural seam mirrors 4-a's
  `SessionKeyUnlocker`.
- **The login-v2 grant request is basic-authenticated** (ADR-0101 §Context):
  `requireAuth` already runs `UnlockForLogin`, so a basic-authenticated
  grant of an enrolled user holds the unlocked private key on the principal
  at `Issue` time. The same holds for a SESSION-authenticated OCS
  `getapppassword` request (the 4-a middleware attaches the session's key) —
  the two moments wrap-at-issuance is possible. The issuer
  previously discarded the `Token` (and with it the token id the wrap row
  keys on).
- **Revocation goes through one store method.** `SQLStore.DeleteByHash`
  serves both the OCS delete endpoint and `RevokeAppPassword`; a hook there
  covers every revocation path. `MemoryStore` (test-only) is unchanged.
- **The uid cache deliberately caches no key material.** The bearer
  verifier's cache stores the uid for 60 s to skip the token-row join; the
  unlocked key is re-opened per request on the hit path too.

## Decision

### 1. Migration 0022 (three dialects in lockstep)

`app_token_keys(app_password_id PK, sealed_uk, salt, created_ms)` — column
types mirror each dialect's `app_passwords.id` (sqlite/postgres `TEXT`,
mysql `VARCHAR(255)`) and 0021 blob conventions. No FK clause, matching the
0020/0021 explicit-lifecycle convention: the four purge paths (§5) and
reconcile's orphan prune own cleanup. Down drops the table.

### 2. Crypto construction (`internal/storage/encrypt/resolver_token.go`)

Byte-exact per ADR-0100 §3: `KEK = HKDF-SHA256(tokenRaw, salt(16 random),
"NCGOAK1" || be64(user_id))` (tokens are ≥256-bit random — no stretching;
argon2id per request would be a self-DoS vector); blob =
`nonce(12) || AES-256-GCM(KEK, privkey(32), same AD)` = 60 B, via the shared
`wrapSeal`/`wrapOpen` and a salted `hkdfSHA256` sibling. The raw token is
never persisted — only the wrap it alone can open.

`WrapKeyForToken(ctx, appPasswordID, tokenRaw, priv)` resolves the owning
user from the `app_passwords` row (unknown id → error; `priv` must be 32
bytes), seals with a fresh salt, and inserts; a unique violation is a no-op
(grant retries are safe). `UnlockForToken(ctx, tokenHash, tokenRaw)` finds
the wrap through the token's STORED hash (`app_token_keys ⋈ app_passwords ON
token_hash`); no row → `nil, nil` (pre-enrollment/imported token — the
verifier attaches no key); an open failure wraps `ErrIntegrity`
(fail-closed, mirroring the 4-a session-copy semantics: a corrupt wrap must
not silently degrade to per-file 403s). `OnTokenDeleted(ctx, appPasswordID)`
deletes the wrap (missing row no-op).

### 3. Wrap-at-issuance (login-v2 grant and OCS getapppassword)

`web.AppPasswordIssuer.Issue` widens to return the token id alongside the
raw token — `(raw, tokenID string, err error)` — and the OCS
`AppPasswordIssuer` interface follows. The
issuer (app routes) now keeps the `Token` it mints. `LoginV2.HandleGrant`:
after a successful `Issue`, when `principal.UnlockedKey != nil` (a
basic-authenticated grant of an enrolled user, unlocked at `requireAuth` in
4-a) the grant seals the key under the new token via the widened
`LoginKeyHandler.WrapKeyForToken`; **a wrap error fails the grant loudly**
(500 "app password key wrap failed") — a silently unwrapped token would 403
every file. With no unlocked key (an app-password-authenticated grant, or an
unenrolled user) the wrap is skipped silently: such tokens authenticate but
cannot unlock files until re-issued from a password login (the pinned
no-wrap behavior).

The OCS `getapppassword` endpoint wraps with the same semantics: its
principal comes from the auth middleware, so a SESSION-authenticated request
of an enrolled user carries the 4-a middleware-attached unlocked key (the
web UI's settings flow), and the handler seals it under the new token via
the narrow `ocs.AppTokenKeyWrapper` seam (constructor-wired from the key
resolver, nil-ok). A wrap error fails the request loudly (server error).
Basic/bearer OCS issuance carries no key and wraps nothing — those tokens
behave like pre-enrollment tokens until re-issued from an unlocked session.

**Same-day correction**: the first cut of this ADR claimed the OCS endpoint
does not wrap, reasoning its principal never carries a key — true only for
basic/bearer auth. Session-authenticated issuance silently produced
wrap-less tokens for enrolled users; the handler now wraps as described
above.

### 4. Verifier unlock (`internal/auth`, structural seam)

`auth.AppTokenKeyUnlocker` (`UnlockForToken(ctx, tokenHash, tokenRaw)`) is
satisfied by `*encrypt.SQLResolver`; auth does not import encrypt.
`AppPasswordVerifier.Keys` (nil-ok): after the existing success path
(including the user-match check), the verifier opens the wrap with
**`t.Hash` — the matched row's stored hash**, so the legacy-hash fallback
needs no second lookup. A nil return attaches nothing; an error propagates
fail-closed. `BearerVerifier.Keys` attaches on BOTH the cache-hit and
cache-miss paths (the uid cache format is untouched); the primary hash is
tried first and `hashTokenLegacy(token)` only when the first call returns
nil (legacy-era tokens) — the two-call pattern the stored-hash keying
requires. Same fail-closed error semantics.

**KEK cache — deliberately NOT implemented** (refines ADR-0100 §7's
parenthetical, strike-annotated there): the construction uses HKDF, so
per-request derive+open costs microseconds, and the extra indexed PK-join is
the same order as the verifier's existing `GetByHash`. A long-lived
key-material cache would add exposure surface for no measurable win.

### 5. Revocation hook and purge coverage

`auth.TokenKeysHook` (`OnTokenDeleted(ctx, appPasswordID)`) on
`auth.SQLStore` (nil-ok; `MemoryStore` unchanged): `DeleteByHash` resolves
the token id, deletes the row, then fires the hook — **best-effort by
contract**: a hook error never fails the delete (the token is already gone),
and `encryption reconcile`'s orphan purge is the safety net. App wiring sets
the hook to the key resolver when non-nil. The wrap row's lifecycle is
covered from every direction: token revocation (the hook), unenrollment at
password login, `DestroyEnrollment` (reset-password `--force`),
`OnUserDeleted`, and `PruneStaleKeys`' new third purge
(`app_token_keys` rows whose app password is gone). `PruneStaleKeys` now
returns `PruneStats{UserKeys, FileKeys, TokenKeys}`; reconcile's output
gains the pruned token-wrap count.

### 6. Status and inventory

`KeyInventory` gains `TokenWraps` (app_token_keys row count) and
`UnwrappedEnrolledTokens` (app passwords of ENROLLED users with no wrap
row). `encryption status` prints `app token key wraps: N` in the per-user
section and, when nonzero, `enrolled users' app tokens without key wrap: N
(re-issue those app passwords — they cannot unlock files)`.

### 7. BrowserLogin inheritance

`BrowserLogin.HandleLogin`: when the basic path produced no key but the
chain verifier attached `principal.UnlockedKey` (an app-password form login
whose token holds a wrap), that key is sealed onto the new session exactly
like a password-unlocked key — an app-password browser login yields a fully
functional session for enrolled users. The 4-a loud-failure behavior for
seal/store errors (500 + session deleted) is unchanged, and app-password
logins still never run `UnlockForLogin`.

### 8. Imported tokens

`import-nextcloud tokens` copies token HASHES only — a wrap needs the raw
token, which Nextcloud never stores. Its help text now states: imported
tokens carry no key wrap; for enrolled users they authenticate but cannot
unlock files — re-issue those app passwords from a password login after the
import.

### Alternatives considered

- **In-memory KEK cache keyed by token hash** (ADR-0100 §7 as written) —
  see §4: HKDF derive+open is microseconds per request; a cache of derived
  key material adds exposure surface and invalidation complexity (rotation,
  revocation) for no measurable win. Rejected; §7 strike-annotated.
- **Wrap-at-issuance inside `auth.IssueAppPassword`** — the auth layer
  never sees the unlocked key (a web-layer value), and pushing key handling
  into the token store would couple auth to encrypt. The issuance handlers
  are the moments both values meet. Rejected.
- **Fail-open verifier** (unlock error → proceed keyless) — silently
  degrades an enrolled user's every file to 403 and masks wrap corruption;
  the 4-a session semantics chose fail-closed for the same reason.
  Rejected.
- **FK clause on `app_token_keys.app_password_id`** — would cascade wraps
  on token/user deletion, but the repo's 0020/0021 convention is explicit,
  tested purges over connection-dependent cascades (sqlite FK enforcement
  is a per-connection pragma the pool would have to guarantee). The hook +
  four purge paths + orphan prune cover the lifecycle without it. Rejected.

## Consequences

- **App-password and bearer DAV clients work for enrolled users again** —
  the 4-a regression window closes. Per-request cost is one indexed
  PK-join + HKDF + AES-GCM open (µs), accepted; no key-material cache
  exists anywhere.
- **Pre-enrollment, imported, and keylessly-issued tokens (app-password
  grants, basic/bearer OCS issuance) authenticate but cannot unlock files**
  until re-issued from a password login or an unlocked session.
  `encryption status` names the count with the remedy; the
  import help says the same.
- **Revocation is cryptographic**: deleting the token deletes its wrap
  (best-effort hook with the orphan purge as safety net), so a captured raw
  token opens nothing after revocation even before it stops verifying.
- **Browser sessions inherit token-wrap keys**: an app-password form login
  whose token holds a wrap produces a session as functional as a password
  login's. Sessions mint no new wraps — a token's wrap stays bound to that
  token.
- **`PruneStaleKeys`' signature changed** to `PruneStats` (callers: the CLI
  reconcile command and tests); `AppPasswordIssuer.Issue` widened in both
  `web` and `ocs`. Zero new third-party dependencies.
- **Rollback**: down-migration drops `app_token_keys`; app-password
  requests of enrolled users return to the 4-a behavior (per-file
  `ErrKeyLocked` → 403). Nothing else moves.

## Verification

- `internal/migrations`: 0022 up/down/up in lockstep (app_token_keys present
  at v22, gone at v21, 0021/0020 objects intact).
- `resolver_token_test.go`: wrap round-trip (60 B blob, 16 B salt, same key
  back; wrong token and tamper → `ErrIntegrity`); unique re-wrap no-op
  (first wrap survives); unknown id and bad key length errors; no-row →
  nil, nil; stored-hash lookup (primary misses a legacy row's wrap, legacy
  hits); `OnTokenDeleted` (idempotent); the four purge paths each removing
  exactly the target user's wraps; inventory token counts; concurrent
  `UnlockForToken` vs `OnTokenDeleted` under `-race` (key or nil, never an
  error).
- `internal/auth`: app-password verifier attaches the key when a wrap
  exists (one call with the stored hash), nil when none, error fail-closed,
  legacy row unlocked by its stored hash, nil `Keys` inert; bearer verifier
  on cache-hit (empty store, uid from cache, wrap still opens) AND
  cache-miss paths, primary-then-legacy two-call order, fail-closed;
  `DeleteByHash` fires the hook with the right id, hook error never fails
  the delete, `ErrTokenNotFound` unchanged.
- `internal/web`: basic grant wraps with (token id, raw token, unlocked
  key) — spy; wrap error → 500 naming the failure; app-password grant wraps
  nothing; app-password form login with a verifier-attached key gets the
  key sealed onto the new session (`UnlockForLogin` never called); 4-a
  login tests keep green.
- `internal/ocs`: session-authenticated getapppassword wraps with (token
  id, raw token, unlocked key) — spy; wrap error → server error; keyless
  (basic/bearer) issuance wraps nothing, token still issued; app-password
  principal still forbidden.
- `internal/files` end-to-end: enrolled user, wrapped app password → GET
  200 with content (verifier → principal → `resolveIdentity` box path);
  bare token → 403; revoke → wrap row gone + token dead; re-issue with wrap
  → 200 again.
- `cmd/ncgo-cli`: status prints the wrap count and the unwrapped-enrolled
  warning line; reconcile prints the pruned token-wrap count (existing
  fixtures).
- Gates: `golangci-lint fmt` clean, `gofmt -l` empty, `go test ./...`
  green, `golangci-lint run ./...` 0 issues, `go mod tidy` no-op (zero new
  dependencies), `go test -race ./...` green.
