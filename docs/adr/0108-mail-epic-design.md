# ADR-0108: Mail epic design (M1: accounts, credential sealing)

- **Status**: Accepted
- **Date**: 2026-10-02
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: (none)

## Context

The v2 Mail epic reimplements the official Nextcloud Mail app's server side:
per-user IMAP accounts, mailbox sync, and message send. The epic ships in
increments; **M1 (this ADR's build scope) lands account foundations only** —
migration 0027 `mail_accounts`, credential sealing, the SQL store, the JSON
REST accounts API, the `mail.enabled` config flag, the capabilities block,
and wiring. Mailbox sync (M2), send (M3), charset hardening (M4), and the
admin surface (M5) are follow-ups this ADR pre-decides so no increment has
to re-litigate them.

Constraints that shaped the design:

- **The zero-new-deps rule is absolute.** Everything below builds on the Go
  stdlib plus dependencies already in `go.mod`; golang.org/x/text (today an
  indirect dep) is the one promotion, and only in M4 (§6).
- **Mail credentials must open without the user present.** Background sync
  runs in a job ctx with no session and no unlocked user key, so the
  per-user-keys encryption module (ADR-0097/0100) — which only exists when
  `encryption.per_user_keys` is on — cannot be the credential store.
- **The repo router has no path parameters** (`internal/httpx/router.go`:
  exact + longest-prefix only), and the CSRF middleware admits unsafe API
  verbs via `OCS-APIRequest: true` or a session cookie + requesttoken
  (ADR-0064). The API surface must fit both.

## Decision

### 1. In-repo IMAP4rev1 client subset; stdlib `net/smtp` for send

M2/M3 implement a minimal IMAP4rev1 client in-repo on `net/textproto` +
`crypto/tls` (capability/LOGIN/SELECT/UID FETCH/UID STORE/LOGOUT — the sync
subset), rather than importing a third-party IMAP library; outbound send
uses the stdlib `net/smtp` client behind the same egress guard (§4). Both
choices keep the epic at zero new third-party dependencies.

（**landed/refined 2026-10-02**: M2 landed the client core as
`internal/mail/imap` — greeting (OK/PREAUTH/BYE), CAPABILITY, LOGIN,
LOGOUT, NOOP, and STARTTLS upgrade, with per-exchange I/O deadlines, a
literal-capable response reader (RFC 3501 `{n}` chaining, without which the
stream desyncs), a token parser foundation (atoms / quoted strings /
parenthesized lists / NIL / literals) for the later ENVELOPE/BODYSTRUCTURE
work, and injection-safe argument quoting (`ErrInjection` on CR/LF before
any write). LIST/SELECT/UID FETCH/UID STORE remain M3/M4 scope. The client
is plain `net.Conn` + `bufio` rather than `net/textproto` — literal
handling needs byte-exact control textproto does not offer.）

（**landed 2026-10-02 (M5)**: send landed exactly as pre-decided — stdlib
`net/smtp` behind the egress-guarded dialer, in package `mail` (no
subpackage; the `DialSMTP` seam is a field on the `Sender` and the
production closure only builds the address from the options). TLS modes
mirror IMAP: `ssl` wraps before the greeting, `starttls` upgrades after
EHLO (a server not advertising it is a 502), and **`none` + credentials is
refused BEFORE AUTH** ("cleartext authentication refused", 502 class;
stdlib `PlainAuth`'s own non-TLS refusal is only the second line). An empty
`SMTPUser` is relay mode: AUTH is skipped entirely. An RCPT refusal is the
typed `ErrRecipientRefused` (400, naming the address); auth/dial/TLS/
transport failures are `ErrUpstream` (502). MIME compose is stdlib-only:
RFC 2047 Q-encoded subjects, quoted-printable text leaves (charset=utf-8),
base64 attachments at 76 columns with RFC 2231 filenames via
`mime.FormatMediaType`, multipart/alternative and multipart/mixed trees,
CR/LF injection guards on every header-destined value, and a 25 MiB
composed cap (typed, shared with M4). **Bcc is envelope-only** — it reaches
RCPT and never the message bytes. Caps: ≤20 attachments, ≤10 MiB decoded
each, ≤25 MiB decoded total. Save-to-Sent is best-effort: when the synced
mailbox list has `special_use='sent'`, the composed bytes are APPENDed with
`\Seen` over a fresh IMAP session (no SELECT needed) — APPEND is the
client's first client-literal command, so it runs its own exchange
supporting both the classic `+ ` continuation and LITERAL+ (`{n+}`); the
fatal-continuation rule stands for every other command. An append failure
logs and reports through the `OnAppendError` hook (`ErrSentAppend`) and the
send result stands. REST: `POST /apps/mail/api/accounts/{id}/send` (JSON
to/cc/bcc/subject/bodyPlain/bodyHtml/inReplyTo/references/attachments) →
200 `{messageId}`. **Files-path attachments landed**: the optional
`{path: "/Documents/file.pdf"}` form (mutually exclusive with
`contentBase64`) resolves against the SENDER's files through the very
`webdav.FS.Read` seam WOPI's GetFile uses — share/ownership checks intact,
unreadable → 400, capped at the 10 MiB per-attachment limit.）

### 2. JSON REST under `/apps/mail/api`, mirroring the official app subset

Pure-JSON endpoints (official field names: `emailAddress`, `imapHost`,
`imapSslMode`, …) mounted session-authed (`webdav.Auth`, the same middleware
as the WOPI mint):

- `POST /apps/mail/api/accounts` — validate-only create (M1 deliberately
  does NOT dial IMAP to verify; that lands in M2) → 201 + account JSON.
  Passwords NEVER appear in any response.
- `GET /apps/mail/api/accounts` → the caller's accounts, `[]` when none.
- `GET /apps/mail/api/accounts/{id}` → one account plus a `"mailboxes": []`
  placeholder for forward-compat (M2 fills it).
- `PUT /apps/mail/api/accounts/{id}` — partial update (pointer fields);
  password halves re-seal only when provided; 404 when not owned.
- `DELETE /apps/mail/api/accounts/{id}` → 200 `{}`; 404 when not owned.

One `MethodAny` prefix route; the handler parses the `{id}` tail itself
(the WOPI `FilesHandler` pattern). Errors are the repo's `{"error": msg}`
JSON convention (console's errorPayload) with 400/401/404/500. Every read
and write is scoped to the caller's uid — a cross-user row is
indistinguishable from a missing one (404, never 403). No CSRF path
bypass: API clients send `OCS-APIRequest: true`, session-cookie verbs keep
the auth middleware's requesttoken check.

### 3. Credential sealing: HKDF(instance.secret) + AES-256-GCM, AD-bound

`SealCredential`/`OpenCredential` (`internal/mail/credentials.go`) are
standalone — no dependency on `internal/storage/encrypt`, which only exists
when per-user-keys is on. Blob = `salt(16) || nonce(12) || ciphertext`;
key = `HKDF-SHA256(instance.secret, salt, "NCGO mail credential v1")`;
AES-256-GCM with AdditionalData
`"NCGOMK1" || 0x00 || userID || 0x00 || imapHost || 0x00 || imapUser` — a
blob copied to another user, host, or login does not open, and any tamper
is one undifferentiated `ErrCredential`. The instance secret is the app's
already-resolved one (generated at boot when unconfigured), so sealing
works on every deployment and **background sync can open credentials
without any user's unlocked key** — the property the per-user-keys module
cannot offer. The single `password_sealed` column packs both password
halves (`be16(len) || imap || smtp`); an empty smtp password means "same as
imap". Because the AD binds host+login, an update that moves `imap_host` or
`imap_user` re-seals the same passwords under the new triple; a stored blob
that fails to open is a loud 500, never a silent credential wipe. The store
only ever sees sealed bytes; the Service seals before `Create`/`Update`.

### 4. Outbound dials reuse the ADR-0057 egress IP guard (M2/M5)

IMAP/SMTP dials (M2 sync, M3 send, and the M2 "verify account" dial) reuse
the `net.Dialer.Control` egress guard (`internal/plugins/egress.go:34-54`):
loopback/private/link-local/unspecified targets are refused at connect
time with no TOCTOU window. Because mail servers legitimately live on LANs
(a home-lab Dovecot), M5 adds an **admin allowlist** of private mail hosts
(config-gated, off by default) that exempts listed host:port pairs from the
guard — the plugin `http.outbound_allow_private` precedent.

（**landed/refined 2026-10-02**: M2 shared the guard primitives via the new
`internal/netx` package (`BlockedEgressIP` / `GuardControl` /
`GuardedDialContext`); `internal/plugins` delegates to it with zero
behavior change — plugins keep their capability-based bypass
(`http.outbound_allow_private` selects the unguarded client) and never use
an allowlist. The M2 "verify account" dial landed: account create (and any
update touching the IMAP connection fields) verifies the LOGIN before
persisting, mapping a refused LOGIN to `ErrVerifyAuth` and every other
failure to `ErrVerifyConnect` (distinct 400s, no row written). The admin
allowlist landed early, in M2 rather than M5, as
`mail.egress_allow_private` — CIDR strings (not host:port pairs) parsed to
`netip.Prefix` at app wiring, invalid CIDRs rejected at config load; empty
default keeps the guard fail-closed.）

### 5. Sync: periodic `mail.sync` fan-out, UID-incremental, poll-only

M2 syncs through the jobs runner: a periodic `mail.sync` job fans out one
per-account child job, each doing UID-incremental fetches
(`UID FETCH <last+1>:*`) against the stored watermark. **Poll-only — no
IMAP IDLE in v1**: IDLE would pin a connection (and a worker) per account
permanently and adds nothing a 5-minute poll doesn't deliver for the v1
feature set. Job rows survive restarts, so a crash mid-sync resumes from
the watermark rather than re-fetching the mailbox.

（**landed 2026-10-02 (M3)**: the sync engine landed in
`internal/mail/sync.go` with migration 0028 (`mail_mailboxes` +
`mail_messages`, no DB-level FKs — account delete cascades at the app
layer inside one transaction). Refinements against the sketch above: one
`mail.sync` job syncs every account **sequentially** (2-minute per-account
ctx) instead of fanning out per-account child rows — simpler and the poll
interval bounds total work; and the incremental fetch is a full uid-set
**diff** (`EXAMINE` — read-only, never SELECT — then `UID SEARCH ALL`
against the local uid list) rather than `UID FETCH <last+1>:*`, so a
server-side delete converges too. New uids fetch summaries in
**500-per-UID-FETCH batches capped at 2000 per mailbox per run** (the
remainder resumes next run through the same diff); a **200-uid flag
refresh window** each run gives poll-mode convergence without CONDSTORE.
A UIDVALIDITY change wipes and re-syncs the mailbox. Still poll-only — no
IDLE, no CONDSTORE in v1 — and v1 accepts the runner's global
`jobs.poll_interval` (no per-job interval). New messages in `INBOX`
publish `mail.message.arrived` (msgpack) only when the mailbox's previous
cursor was non-zero, so the initial bulk sync never rings; the app
subscriber inserts one bell notification per event. Landed REST:
`GET /apps/mail/api/accounts/{id}/mailboxes` (counts + mUTF7-decoded
display names; GET-one-account's placeholder now returns the same synced
list) and `POST /apps/mail/api/accounts/{id}/sync` (synchronous pass,
200 `{newMessages}` / 502). Message body fetch/list APIs are M4.）

### 6. `golang.org/x/text` promoted to direct in M4 (charset decoding)

Message bodies arrive in arbitrary charsets; M4 (message rendering)
promotes golang.org/x/text — already an indirect dependency — to a direct
one for `encoding/htmlindex`-driven decoding. M1–M3 need no text decoding
and add nothing.

（**landed 2026-10-02 (M4)**: the message APIs landed with exactly this one
dependency exception — x/text moved indirect → direct, `go.sum` untouched.
The imap client grew the write commands: `SELECT` (read-write, sharing
EXAMINE's parsing; a tagged NO is the typed `ErrCommandRefused`),
whole-message `UID FETCH <uid> (UID BODY.PEEK[])` (the literal-capable
reader counts octets, so CRLF/NUL/`{n}`-looking payloads cannot desync it),
`UID STORE` behind a strict flag allowlist (system flags + keywords;
anything else rejected before any write), `UID COPY`, and `EXPUNGE`.
Bodies decode through `DecodeBody` (htmlindex labels; unknown/unsupported
charsets pass through as UTF-8, never error) and a whole-message MIME walk
over stdlib net/mail + mime/multipart: first plain/html leaf wins,
attachments are indexed depth-first (detail and download share the index),
malformed trees return what parsed, >32 MiB is a typed refusal. Live ops
run over **per-request connections** (dial → LOGIN → SELECT → op → LOGOUT)
— connection pooling is a documented follow-up. REST:
`GET .../mailboxes/{mbid}/messages` (keyset cursor `<dateUnix>_<id>`,
default 50/max 200), `GET .../messages/{mid}` (live fetch; marks \Seen by
default, `?markSeen=false` skips; response carries `"htmlSanitized":
false`), `PUT .../messages/{mid}/flags`, `DELETE .../messages/{mid}`
(trash-COPY when a synced trash mailbox exists, else in-place expunge),
`PUT .../messages/{mid}/move` (local row DELETED — the next sync
rediscovers the copy in the destination), and
`GET .../messages/{mid}/attachments/{index}`. **The delivered HTML is
UNSANITIZED: a sanitizer is a REQUIRED follow-up before any first-party
HTML UI** — clients must sanitize until then. List preview and
has-attachments flags are follow-ups too (they need
`BODY.PEEK[TEXT]`/BODYSTRUCTURE in the sync fetch shape).）

### M1 build surface (what this increment lands)

Migration 0027 `mail_accounts` in three dialects (sqlite/mysql/postgres in
lockstep; `id` autoincrement PK, `user_id` TEXT, the imap/smtp column set
with `ssl`/`starttls`/`none` modes defaulting to `'ssl'`, `password_sealed`
BLOB, `created_at`/`updated_at` unix seconds, index on `(user_id, id)`;
down drops the table — the explicit-lifecycle convention, no FK cascade);
`internal/mail` (credentials, store, service, handler); `mail.enabled`
config (default false, env `NCGO_MAIL__ENABLED`); the `{"mail":{"enabled":
true}}` capabilities block registered only when enabled; app wiring that
builds the service only when enabled — flag off leaves the server
bit-identical to pre-mail builds.

## Alternatives considered

- **Third-party IMAP library (e.g. emersion/go-imap)** — rejected: the
  zero-new-deps rule; the sync subset (login/select/UID fetch/store) is a
  few hundred lines on `net/textproto`, and upstream compatibility risk is
  lower with a client we control.
- **Seal credentials with the per-user-keys module** — rejected: the module
  only exists under `encryption.per_user_keys` and its opens need the
  user's unlocked key (session ctx). Background sync has neither; an
  instance-secret seal is the only construction that works on every
  deployment, and the AD triple keeps blobs non-portable across users and
  hosts.
- **OCS envelope for the API** — rejected: the official Mail app's API is
  plain JSON, not OCS-wrapped; mirroring it keeps future client-compat
  discussions anchored to upstream, while the repo's `{"error"}` JSON
  convention covers failures.
- **IMAP IDLE push in v1** — rejected: one pinned connection + worker per
  account is a fleet-scaling problem; poll-only UID-incremental sync is
  restart-safe and operationally boring. IDLE can be revisited as an opt-in
  after v1.
- **Eager IMAP verification at account create (M1)** — deferred to M2:
  verification needs the egress-guarded dialer (§4) and a failure taxonomy
  (auth vs. unreachable vs. TLS) that M1's scope deliberately excludes.

## Consequences

- **M1 ships a complete accounts CRUD surface** with sealed-at-rest
  credentials; enabling `mail.enabled` mounts the API and advertises the
  capabilities block, disabling removes both with zero residue.
- **Credential confidentiality rests on the instance secret** (already the
  root of app-password and request-token security); an attacker with DB
  read but not the secret gets nothing, and blobs are non-portable across
  users/hosts/logins. Rotation of the instance secret invalidates sealed
  mail passwords (documented operational trade-off, same class as
  app-password invalidation).
- **M2+ inherits fixed seams**: the egress-guarded dialer, the `mail.sync`
  job name, the `mailboxes` placeholder, and the packed password pair. The
  `imap_password`/`smtp_password` pair never needs a schema change.
- **Rollback**: down-migration drops `mail_accounts`; flag off unmounts
  everything. Nothing else moves.
- Zero new third-party dependencies (HKDF is the already-direct
  golang.org/x/crypto; AES-GCM/HKDF idioms mirror ADR-0102/0107).

## Verification

- `internal/migrations`: 0027 up/down/up in lockstep (mail_accounts present
  at v27, gone at v26, wopi_token_keys intact).
- `internal/mail/seal_test.go`: seal/open round-trip (incl. empty
  plaintext), randomized salt/nonce, and tamper matrix — wrong secret,
  wrong uid/host/login, empty/short/truncated blob, flipped salt and
  ciphertext bytes — all one `ErrCredential`; password-pair pack/split.
- `internal/mail/sqlstore_test.go`: full CRUD against in-memory sqlite with
  the real migration chain; user scoping (bob cannot see/update/delete
  alice's row); ssl-mode defaults; invalid-row rejection.
- `internal/mail/handler_test.go`: table-driven httptest behind the REAL
  `webdav.Auth` middleware — create/list/get/update/delete, cross-user 404
  on every verb, the 400 validation matrix (required fields, port range,
  ssl enum, net/mail email parse), every response asserted password-free,
  sealed-at-rest (raw row read: blob ≠ plaintext, opens back to both
  halves), and host-change reseal (new AD opens, old AD fails).
- `internal/app`: mail.enabled mount gate (401 unauthenticated proves the
  mount; full-router create+list with `OCS-APIRequest: true`; capabilities
  block present only when enabled); one golden replay case
  (`testdata/golden/mail/001-list-empty`).

## References

- ADR-0057 (egress IP guard — §4's reuse), ADR-0064 (requesttoken CSRF),
  ADR-0102/0107 (the HKDF+AES-GCM wrap idioms §3 mirrors), ADR-0106 (WOPI —
  the session-auth mount and handler-dispatch patterns), docs/
  CHUNKED_UPLOAD_V2_SPEC.md-era v2 epic convention.
