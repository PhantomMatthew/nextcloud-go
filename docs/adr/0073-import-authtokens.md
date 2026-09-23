# ADR-0073: Phase 5a import-nextcloud tokens — app passwords (oc_authtoken)

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0071 (resolves its `tokens` follow-up)

## Context

ADR-0071 §Decision 3 delivered the verdict on sessions and app passwords and
pinned the design of a future `import-nextcloud tokens` subcommand, but
deliberately shipped no import code (research + guidance only, with a manual
SQL recipe as the stopgap). This increment (5a) ships that subcommand; the
design is the one ADR-0071 pinned, restated here as implemented.

The verified facts that make this work (ADR-0071 §research, against the
Nextcloud server source tree): `oc_authtoken.token` is
`hash('sha512', $token . $secret)` with the instance's config.php `secret`,
identical in the current `PublicKeyTokenProvider` and the pre-24
`DefaultTokenProvider`; token `type` is `0 = temporary` (browser sessions),
`1 = permanent` (app passwords), `2 = wipe` (`IToken.php`). ncgo deliberately
mirrors all of it: `internal/auth.HashToken` is the same
`hex(sha512(token+secret))`, the type constants `0/1/2` match, and
`AppPasswordVerifier.Verify` is a pure token-hash lookup — so a copied
`oc_authtoken.token` value verifies a client presenting the original token
if and only if ncgo's `instance.secret` equals the source instance's
`secret`.

## Decision

`import-nextcloud tokens` (`cmd/ncgo-cli/importnc_tokens.go`) reads
`SELECT id, uid, login_name, name, token, last_activity FROM
<prefix>authtoken WHERE type = 1 ORDER BY id` and inserts one `app_passwords`
row per mappable token via `auth.SQLStore.Insert`. Column mapping:

| Source `oc_authtoken` | ncgo `app_passwords` | Notes |
|---|---|---|
| `id` | `id` | synthesized as `nc-<id>` (source ids are integers; the prefix keeps them distinct from ncgo-issued token ids, which are the first 16 chars of the raw token) |
| `token` | `token_hash` | **verbatim** — the whole point; NULL/empty → skip + warn |
| `uid` | `user_id` | via the uid mapping below; Insert resolves uid → `users.id` |
| `login_name` | `login_name` | NULL/empty → the mapped target uid |
| `name` | `name` | NULL → empty string |
| — | `type` | constant `1` (`auth.TokenTypePermanent`) |
| `last_activity` | `created_at` | unix seconds → `time.Unix(last_activity, 0)`, stored as ms; NULL → 0 |
| `password`, `password_hash`, keypair columns, `scope`, `expires`, … | — | **dropped**: ncgo never decrypts stored passwords, and `app_passwords` has no counterpart columns (ADR-0071 recipe said the same) |

**uid mapping** reuses the users importer's rule exactly
(`importNCUIDMap`): read `<prefix>users`, target = `uid` when
`validNCImportUID(uid)`, else `uid_lower` when valid, else the row is
unmappable. Tokens whose uid is unmappable are skipped with a per-row
warning naming the source row id.

**Mandatory user-existence pre-check.** `auth.SQLStore.Insert` is
`INSERT ... SELECT ... FROM users u WHERE u.uid = ?` — for an unknown uid it
inserts **0 rows and returns a nil error**, so a token for a missing user
would vanish silently and, worse, be counted as created. The importer
therefore calls `users.SQLStore.GetByUID` first; `ErrNotFound` → skip +
warning (run `import-nextcloud users` first), any other error → hard fail of
the run.

**Idempotency** is a hash lookup, not the `nc-<id>`: `GetByHash(token)` →
found means skipped (existing), `ErrTokenNotFound` means proceed, any other
error hard-fails the run. Matching on the hash (the table's UNIQUE column)
also skips tokens the operator carried over by hand with the ADR-0071 SQL
recipe, regardless of the id they used.

**Sessions verdict unchanged.** Browser sessions (type 0) remain not
importable — ncgo sessions are their own token family — and are filtered in
SQL together with wipe tokens (type 2); users log in again.

**Report and dry-run.** Entity name `app passwords`; per-row warnings
identify the source row id. `--dry-run` scans and counts (resolving uid
mapping, target-user existence, and hash collisions) but writes nothing. A
missing or unreadable `<prefix>authtoken` table is a **hard error**,
consistent with the users importer's required tables (the source claims to
be a Nextcloud database; authtoken has existed since Nextcloud 9's token
auth rewrite, so its absence means a wrong DSN/prefix more often than a
genuinely old schema).

**Help-text contract** (parent command and subcommand `Long`): only type-1
app passwords are imported; imported tokens verify **only** when ncgo's
`instance.secret` equals the source config.php `secret`, copied **before
first use and kept forever** — rotating the secret invalidates every
imported token at once; operators who keep a fresh secret (recommended for
production) skip this import and have users re-issue app passwords.

## Alternatives Considered

### Reading all authtoken rows and filtering types in code
- Pros: the report could count sessions/wipe tokens as skipped, making the
  filter visible; one less assumption in SQL.
- Cons: sessions dominate real `oc_authtoken` tables (every browser login
  is a row) and none of them can ever be imported — counting them would
  bury the app-password numbers in noise and produce thousands of pointless
  skip warnings. The `WHERE type = 1` filter is the pinned ADR-0071 design;
  skipped-not-counted matches how the shares importer treats unsupported
  remote-share rows' tokens. SQL filter kept.

### Warning instead of failing on a missing authtoken table
- Pros: mirrors `readNCUserExtras`, where missing optional tables
  (`oc_accounts` on pre-13 sources) degrade to one warning.
- Cons: `authtoken` is not an optional extra table — it is the *only* input
  this subcommand reads; a warning would report "0 created" for what is
  almost certainly an operator error (wrong DSN or table prefix). The users
  importer hard-fails its required tables the same way. Hard error kept.

### Idempotency on the synthesized `nc-<id>` instead of the hash
- Pros: detects a re-import of the same source row even if the hash
  changed (it cannot — the hash is the payload).
- Cons: misses tokens imported by hand via the ADR-0071 recipe (arbitrary
  ids) and tokens re-issued through ncgo with the same hash, and `id` has a
  UNIQUE constraint collision mode of its own. The hash is the natural key
  (`token_hash UNIQUE`) and the value the verifier looks up. Hash lookup
  kept.

### Importing `last_activity` into a separate column / ignoring it
- Cons: ncgo `app_passwords` has `created_at` (NOT NULL) and nullable
  `last_used_at`; the source has no creation timestamp at all —
  `last_activity` is the closest faithful value, exactly what the ADR-0071
  SQL recipe used (`t.last_activity * 1000`). Setting `last_used_at` from
  it was rejected: ncgo's auth layer owns that column's update semantics
  and a NULL start is the honest "never used here" state.

## Consequences

- New CLI surface: `ncgo-cli import-nextcloud tokens` (+ the shared
  `--source-driver/--source-dsn/--table-prefix/--dry-run` flags). The
  parent command help now lists `tokens` and points at it; the `users`
  help's closing line points there too. ADR-0071's manual SQL recipe
  remains valid for operators who already used it — the subcommand skips
  those rows on their hash.
- The import copies hashes, never plaintext tokens: ncgo still cannot
  recover the original token from the database, exactly like Nextcloud.
- **Secret-rotation hazard, by design**: every imported token's validity is
  bound to `instance.secret`. Copying the source secret before first use
  and then rotating it later invalidates all imported tokens (and all
  ncgo-issued ones) at once — the help texts and this ADR state it; there
  is no per-token re-hash path because the plaintext tokens are gone.
- The `Insert` 0-rows-on-unknown-uid behavior is now documented at its call
  site here; the pre-check pattern is the one to copy for any future
  importer writing through `auth.SQLStore`.
- No import-nextcloud follow-ups remain open after this increment.

## Verification

- `cmd/ncgo-cli/importnc_tokens_test.go`:
  - `TestImportNCTokens` — happy path on a sqlite source fixture
    (`oc_users` + `oc_authtoken`): report line `app passwords: 2 created, 3
    skipped (existing), 0 failed` with per-row warnings for the
    missing-target-user, unmapped-uid, and NULL-token rows; the imported
    row asserted via `GetByHash` (id `nc-1`, verbatim hash, login_name,
    name, type permanent, `created_at` = `last_activity` seconds → ms);
    **compatibility money test**: the fixture stores `HashToken(rawToken,
    testSecret)`, and `AppPasswordVerifier` with the same secret
    authenticates `rawToken` (principal uid + app-password method), while a
    verifier with a different secret rejects it — the cross-instance
    carry-over claim in both directions; type-0 session and type-2 wipe
    rows verified absent; uid_lower fallback row lands for the mapped uid
    with NULL login_name/name/last_activity tolerances; re-run is
    all-skipped with 0 created and an unchanged row count.
  - `TestImportNCTokensDryRun` — same counts, `dry-run: no changes
    written`, zero rows in `app_passwords`.
  - `TestImportNCTokensMissingTable` — a source without `authtoken` fails
    the run with `read source authtokens`.
- Gates: `golangci-lint fmt ./...` + `gofmt -l internal/ pkg/ cmd/` empty,
  `go test ./...` green, `golangci-lint run ./...` 0 issues, `go mod tidy`
  no diff, `go test -race ./cmd/ncgo-cli/ ./internal/auth/` green.
