# ADR-0071: Phase 4y import-nextcloud completion — calendar shares, user email/quota, authtoken verdict

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0048 (resolves its email/quota follow-up), ADR-0051
  (resolves its calendar-shares follow-up)

## Context

The 4e import series (ADR-0048/0049/0050/0051) deferred three items:

1. user **email and quota** (ADR-0048 left `oc_preferences`/`oc_accounts`
   out of scope for v1),
2. **calendar shares** (ADR-0051 §6 counted them with a warning, fearing
   invite-state and group-principal mismatches),
3. a conclusive verdict on **sessions and app passwords**
   (`oc_authtoken`), which ADR-0048 §5 ruled out without full research.

This increment (4y) closes all three. The schema claims below were verified
against the Nextcloud server source tree and its release branches (URLs
inline; local snapshot at `nextcloud-server` master, plus `stable9`–
`stable12` and sabre-io/dav fetched for history).

## Source-schema research (verified facts)

### Users: email and quota do NOT live in `oc_users`

`oc_users` has had exactly `uid, displayname, password` since Nextcloud 13,
plus `uid_lower` since 14 — no email, no quota column, in any release
([core/Migrations/Version13000Date20170718121200.php](https://github.com/nextcloud/server/blob/master/core/Migrations/Version13000Date20170718121200.php),
[Version14000Date20180404140050.php](https://github.com/nextcloud/server/blob/master/core/Migrations/Version14000Date20180404140050.php)).

- **Quota** is a preference: `oc_preferences` row `(userid, 'files',
  'quota')`. `OC\User\User::getQuota()` reads exactly that key with
  `'default'` as the absent-value default, and `setQuota()` normalizes
  writes through `OC_Helper::humanFileSize` except the two sentinels
  `'none'` (unlimited) and `'default'` (fall through to the `files`
  app `default_quota` appconfig value)
  ([lib/private/User/User.php](https://github.com/nextcloud/server/blob/master/lib/private/User/User.php)
  `getQuota`/`setQuota`). Sizes are 1024-based with optional unit suffix
  (`b/k/kb/m/mb/g/gb/t/tb/p/pb`, case-insensitive, float allowed, bare
  number = bytes) per
  [OC_Helper::computerFileSize](https://github.com/nextcloud/server/blob/master/lib/private/legacy/OC_Helper.php).
  A literal `"0"`/`"0 B"` row is possible and means *zero* quota (no
  storage), distinct from unlimited.
- **Email** has up to three homes, in ncgo's resolution order:
  `oc_preferences` `(uid, 'settings', 'primary_email')` — what
  `User::getPrimaryEMailAddress()` reads — then `(uid, 'settings',
  'email')` (the legacy "system" email `getSystemEMailAddress()` falls back
  to; `getEMailAddress()` is primary ?? system), then the `email` property
  of the `oc_accounts.data` JSON blob (`oc_accounts(uid, data)` exists
  since Nextcloud 13; the blob is keyed by property name,
  `{"email": {"value": "...", "scope": "...", "verified": "..."}}`, see
  [lib/private/Accounts/AccountManager.php](https://github.com/nextcloud/server/blob/master/lib/private/Accounts/AccountManager.php)
  `prepareJson`/`importFromJson` and
  [lib/public/Accounts/IAccountManager.php](https://github.com/nextcloud/server/blob/master/lib/public/Accounts/IAccountManager.php)
  `PROPERTY_EMAIL = 'email'`). Additional addresses live in the
  `additional_mail` collection (`COLLECTION_EMAIL`) and are deliberately
  not imported — ncgo users have a single email.

### Calendar shares: `oc_dav_shares` is the only table Nextcloud ever had

Every Nextcloud release (9 through master) stores calendar **and**
addressbook shares in `oc_dav_shares(id, principaluri, type, access,
resourceid)` (`publicuri` added later for public calendar links) —
[apps/dav/lib/Migration/Version1004Date20170825134824.php](https://github.com/nextcloud/server/blob/master/apps/dav/lib/Migration/Version1004Date20170825134824.php)
and the NC9
[apps/dav/appinfo/database.xml](https://github.com/nextcloud/server/blob/stable9/apps/dav/appinfo/database.xml).
The `oc_calendarshares` table the 4e4 fixture assumed **never existed in
any Nextcloud server release** (verified: no match in migrations or
`database.xml` on master, `stable12`, `stable11`, `stable9`; legacy
ownCloud calendar-app tables were `oc_calendar_share_calendar` /
`oc_calendar_share_event`, a different shape again). The importer keeps
reading a `calendarshares`-named table defensively (same mapping,
`calendarid` read as the resource id, type implied `calendar`), but
`dav_shares` is the real schema.

Semantics ([apps/dav/lib/DAV/Sharing/Backend.php](https://github.com/nextcloud/server/blob/master/apps/dav/lib/DAV/Sharing/Backend.php)):

- `access`: `1 = ACCESS_OWNER, 2 = ACCESS_READ_WRITE, 3 = ACCESS_READ`
  (constants at the top of the file; **not** a bitmask and **not**
  intuitively ordered).
- `type`: the resource family, `'calendar'` or `'addressbook'`
  (`$resourceType` constructor argument of the two backends) — so yes,
  `dav_shares` also covers addressbooks.
- `resourceid`: the **integer id** of the row in `oc_calendars` /
  `oc_addressbooks` (`$shareable->getResourceId()`), not the uri — the
  importer joins it through the source calendar rows it just imported.
- `principaluri`: `principals/users/<uid>`, `principals/groups/<gid>`, or
  `principals/circles/<id>` (`shareWith` whitelists exactly these three).
- **There is no invite/accept state.** `getShares()` hardcodes
  `'status' => 1` (accepted) for every row; a row in `dav_shares` *is* an
  effective, visible share. (sabre/dav's own sharing model does have
  invite states — `INVITE_NORESPONSE/ACCEPTED/DECLINED/INVALID` and
  `ACCESS_NOTSHARED/SHAREDOWNER/READ/READWRITE/NOACCESS` in
  [lib/DAV/Sharing/Plugin.php](https://github.com/sabre-io/dav/blob/master/lib/DAV/Sharing/Plugin.php),
  materialized as `share_invitestatus` in its `calendarinstances` table
  ([examples/sql/mysql.calendars.sql](https://github.com/sabre-io/dav/blob/master/examples/sql/mysql.calendars.sql))
  — but Nextcloud never adopted that table; ADR-0051's invite-state
  concern does not apply to the schema Nextcloud actually uses.)

### `oc_authtoken`: hash-compatible by ncgo design

`oc_authtoken.token` = `hash('sha512', $token . $secret)` where `$secret`
is the instance's `config.php` `secret` — identical in the current
[PublicKeyTokenProvider.php](https://github.com/nextcloud/server/blob/master/lib/private/Authentication/Token/PublicKeyTokenProvider.php)
(`hashToken`, plus the `password` column holding the user password
RSA-encrypted to a per-token keypair whose private key is itself encrypted
with `$token . $secret`, and `password_hash` = hasher over
`sha1($pw) . $pw`) and in the pre-24
[DefaultTokenProvider.php](https://github.com/nextcloud/server/blob/stable23/lib/private/Authentication/Token/DefaultTokenProvider.php).
Token `type`: `0 = temporary` (browser session tokens), `1 = permanent`
(app passwords), `2 = wipe`
([lib/private/Authentication/Token/IToken.php](https://github.com/nextcloud/server/blob/master/lib/private/Authentication/Token/IToken.php)).

**ncgo deliberately mirrors this**: `internal/auth.HashToken(token,
secret)` is the same `hex(sha512(token+secret))`, `hashTokenLegacy` is
NC's empty-secret fallback, the 72-char alphabet, and the type constants
`0/1/2` all match (`internal/auth/apppassword.go`). App-password
verification is a pure token-hash lookup
(`AppPasswordVerifier.Verify`), so **an `oc_authtoken` row carried into
ncgo's `app_passwords` table verifies a client presenting the original
token — provided ncgo's `instance.secret` equals the source instance's
`secret`.**

## Decision

### 1. users: email and quota mapped

`import-nextcloud users` now reads `oc_preferences` (quota +
both email keys) and `oc_accounts` (email fallback, JSON-parsed) up front
and applies the resolved values to every **created** user (existing users
are still skipped untouched — re-runs never overwrite):

| Source | ncgo `users` | Notes |
|---|---|---|
| `oc_preferences` `settings/primary_email` | `email` | wins when non-empty (NC's own primary) |
| `oc_preferences` `settings/email` | `email` | legacy system email, second |
| `oc_accounts.data` `email.value` | `email` | fallback when no preference email |
| `oc_preferences` `files/quota` | `quota_bytes` | see quota table |
| `oc_accounts.data` `additional_mail` | — | dropped (ncgo has one email) |

Quota mapping (`parseNCQuota`, mirroring `computerFileSize`'s 1024-based
units): `none` / `default` / empty → `NULL` (ncgo has no instance default
quota, so `default` degrades to unlimited — recorded as a deliberate
widening; admins who relied on a finite NC `default_quota` must set those
quotas after import); human sizes (`"5 GB"`, `"512 MB"`, `"1.5 GB"`,
`"2 TB"`, bare bytes `"1073741824"`, `"0"` → 0) → bytes; unparseable
(`"plenty"`, negative, unknown unit) → `NULL` **plus a per-user warning**
— never a failed row. Malformed `oc_accounts.data` JSON warns per user and
falls back to preference-only email. Missing tables (pre-13 sources have
no `oc_accounts`) degrade to one warning, not a failed run.

### 2. dav: calendar shares imported for the unambiguous subset

`warnNCCalendarShares` is replaced by a mapping pass over **both** share
tables when present (`dav_shares` real schema; legacy `calendarshares`
shape read with the same mapping — defensive, see research). Each row is
classified:

| Source row | Outcome |
|---|---|
| `type = 'addressbook'` | **not imported** (ncgo has no addressbook sharing — `internal/contacts` has no share table, `Shareable: false`): counted in a separate `addressbook shares` entity + one summary warning |
| `type` other/`NULL` | skip + warn (unknown type) |
| principal `principals/groups/*`, `principals/circles/*` | skip + warn (ncgo `calendar_shares.target_user_id` is a single user; expanding groups would change membership semantics after import) |
| sharee uid not in target | skip + warn |
| `resourceid` not in `oc_calendars` | skip + warn (orphan row) |
| source calendar **not imported** (unknown owner, non-user owner principal, uri collision, create failure) | skip + warn — the share pass resolves against the *resolved set* recorded by the calendar pass, so a share can **never attach to a foreign calendar that merely owns the same uri** (ADR-0051's never-show-unaccepted-calendars principle, generalized) |
| sharee == owner | skip + warn (NC refuses self-shares; ncgo's Share endpoint does too) |
| `access = 3` | `calendar_shares.access = 'read'` |
| `access = 2` | `'read-write'` |
| `access = 1` (owner), `0`, `NULL`, other | skip + warn (no share meaning) |
| fully mappable | `UpsertCalendarShare`: **created** when absent, **skipped** when present with the same access, **updated** when present with different access (the store's Upsert semantics — source wins, exactly what re-sharing through ncgo's own CalDAV POST does) |

Invite-state faithfulness is structural, not assumed: NC keeps no pending
state in `dav_shares` (every row is an effective share) and ncgo shares
take effect immediately as well, so importing the row reproduces NC
behavior exactly. The report gains `calendar shares` (created/updated/
skipped/failed via the shared `importReport`) and `addressbook shares`
(skipped-only) lines; per-row warnings identify each skip by table and row
id. `--dry-run` resolves calendars planned-but-not-written as importable
(target id 0 — their shares count as created, since none can exist yet)
and writes nothing.

### 3. Sessions and app passwords: verdict and migration guidance

**Verdict — sessions: not importable.** ncgo browser sessions are its own
`internal/session` token family behind the `nc_session_id` cookie; NC's
temporary authtokens are entangled with PHP's session storage and NC's
request-token derivation. There is nothing to map. Users log in again.

**Verdict — app passwords: faithfully importable in principle, import code
deliberately not shipped in this increment.** Because ncgo's token hashing
is byte-identical to NC's (research above), copying rows works when — and
only when — the operator carries the source `config.php` `secret` into
ncgo's `instance.secret` (`NCGO_SECRET`) **before first use and keeps it
forever** (rotating the secret invalidates every imported token at once).
The 4y scope is research + guidance; a future `import-nextcloud tokens`
subcommand is a small, well-specified follow-up (read `oc_authtoken`
`WHERE type = 1`, map uid → user, copy `token` → `token_hash`,
`login_name`, `name`, `type`; synthesize an `id`; skip rows whose user is
missing). Until it ships, the manual recipe for small instances:

```sql
-- run against the ncgo database after users import, with instance.secret
-- set to the source secret:
INSERT INTO app_passwords (id, user_id, token_hash, login_name, name, type, created_at)
SELECT 'nc-' || t.id, u.id, t.token, t.login_name, t.name, 1, t.last_activity * 1000
FROM oc_authtoken t JOIN users u ON u.uid = t.uid   -- oc_authtoken attached/copied from the source
WHERE t.type = 1;
```

(adjust timestamp units and the id synthesis to the target dialect; the
`password`/`password_hash`/keypair columns have no ncgo counterpart — ncgo
never decrypts stored passwords — and are dropped.)

**Default guidance remains re-issue**: operators who do not copy the
secret (recommended for production: a fresh secret per install) have users
log in and re-create app passwords via the web UI or login flow v2 — the
same flow ADR-0048 already documented. The parent command's long help
states both paths.

## Alternatives Considered

### Importing group-principal shares by expanding members
- Pros: group-shared calendars appear for members.
- Cons: expansion is a snapshot — members added later in ncgo would never
  see the calendar, and members removed would keep it: silently wrong
  either way. Group calendar sharing belongs in ncgo's calendar subsystem
  first. Skipped + warned instead.

### Attaching shares by owner+uri without the resolved set
- Pros: simpler; shares of pre-existing (pre-import) calendars would map.
- Cons: a uri **collision** (ADR-0051 decision 7: different properties,
  skipped wholesale) would attach the share to the foreign calendar —
  exactly the "sharee sees a calendar that was never shared with them"
  failure. The resolved set costs one map and closes it.

### Skipping (not updating) shares whose access differs
- Pros: pure insert-only idempotency like users/groups.
- Cons: hides genuine source/target divergence; `UpsertCalendarShare`'s
  update is the production semantic (re-share = update). The `updated`
  counter in the report line surfaces it without per-row noise.

### Importing additional_mail addresses into oc_preferences-style storage
- Cons: ncgo has no such storage and a single email field. Out of scope.

## Consequences

- `import-nextcloud users` output is unchanged in shape; created users now
  carry email/quota. Operators see new warnings for unparseable quota
  values and broken accounts JSON.
- `import-nextcloud dav` now prints `calendar shares` and (when present)
  `addressbook shares` lines and writes `calendar_shares` rows; re-runs
  are all-skipped; access divergence shows as `N updated`.
- ADR-0048's email/quota follow-up and ADR-0051's calendar-share follow-up
  are resolved; the `tokens` subcommand is the only remaining
  import-nextcloud follow-up, with its design pinned here.
- Operators must be told (command help + this ADR): default path = fresh
  secret + re-login + reissued app passwords; carry-over path = copy
  `secret` + manual SQL (or the future subcommand).

## Verification

- `cmd/ncgo-cli/importnc_test.go`: fixture gains `oc_preferences` /
  `oc_accounts`; alice (settings email + 5 GB), bob (`none` → NULL),
  nodisplay (accounts-only email) asserted on the created rows;
  `TestImportNCUsersEmailQuota` covers the quota matrix
  (none/default/5 GB/512 MB/0/bare bytes/invalid) and every email source
  including primary_email precedence and malformed accounts JSON, report
  counts, idempotent re-run, and dry-run (counts equal, zero writes);
  `TestParseNCQuota` / `TestNCAccountEmail` unit tables.
- `cmd/ncgo-cli/importnc_dav_test.go`: legacy `calendarshares` fixture
  with one importable read share plus every skip category (group,
  unknown resourceid, sharee missing, calendar not imported, access 1,
  self-share); bob's `ListSharedCalendars` read path asserted; re-run
  all-skipped; dry-run counts + zero `calendar_shares` rows.
- `cmd/ncgo-cli/importnc_dav_shares_test.go`: real `dav_shares` schema —
  read-write create, access update-down (pre-existing read-write → read),
  group/circle/unknown-sharee/unknown-calendar/not-imported (collision)
  skips, NULL access, addressbook row counted with summary warning;
  idempotent re-run; dry-run leaves the pre-existing share untouched.
- `cmd/ncgo-cli/importnc_dav_edge_test.go`: `dav_shares` fixture corrected
  to the real schema (type/resourceid); `importnc_dav_broken_test.go`
  still passes with no share table at all (no report line).
- `go test ./...` green, `go test -race` clean, `golangci-lint run ./...`
  0 issues, `gofmt -l internal/ pkg/ cmd/` empty, `go mod tidy` no diff.
