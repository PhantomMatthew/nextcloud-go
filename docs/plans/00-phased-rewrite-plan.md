# Phased Rewrite Plan

**Status**: 🟢 Accepted
**Last updated**: 2026-04-29
**Owner**: Project lead

## Executive Summary

Rewrite Nextcloud Server (PHP, ~1.5M LOC including bundled apps) in Go as a
**greenfield** project, achieving wire-level compatibility with existing
Nextcloud desktop, iOS, Android, and CalDAV/CardDAV clients across five phases
spanning approximately 24 calendar months and ~80 engineer-months of effort.

## Source Codebase Snapshot (PHP Reference)

Captured from `/Users/matthew/SourceCode/github/nextcloud-server` during initial
exploration:

| Area | Approximate LOC | Notes |
|---|---|---|
| `lib/private/` | ~250,000 | Core subsystems (Files, User, DB, AppFramework, Auth, Encryption, Sharing, Jobs, Cache, Search, Preview) |
| `lib/public/` | ~50,000 | Public API surface that apps depend on |
| `apps/` (bundled) | ~199,864 | 28 first-party apps |
| Largest app: `apps/dav/` | 51,925 | WebDAV/CalDAV/CardDAV — replaces SabreDAV abstractions |
| HTTP/CLI entry points | — | `index.php`, `remote.php`, `public.php`, `ocs/v{1,2}.php`, `cron.php`, `occ`, `status.php` |

## Strategic Decisions (Locked)

These were decided up-front and frame every subsequent design choice:

1. **Greenfield, not transpilation.** Fresh Go architecture, fresh DB schema. No
   attempt to run alongside PHP. PHP→Go data migration ships as `ncgo-cli import-nextcloud`
   in Phase 4.
2. **Feature parity with bundled apps**, not just core. Calendar, Contacts, Sharing,
   Federation, Admin, Theming all in scope.
3. **WASM plugin system** for third-party extensibility ([`wazero`](https://wazero.io)).
   No CGO, no native plugin loading.
4. **Wire compatibility** is mandatory. Every existing Nextcloud client must work
   without modification. Validated via captured-traffic golden tests.
5. **No CGO** unless absolutely required (no candidates identified so far).

See ADR-0001 for the rationale tying these together.

## Phases

### Phase 0 — Planning & Foundations (8–10 weeks, 2 engineers)

**Goal**: Boot a binary, lock the architecture, prove the contract testing approach.

- Repository scaffolding (Go module, layout, CI, ADRs, container images)
- Core interface design (DB, Storage, Cache, Auth, Jobs, EventBus, Plugin Host)
- Tech stack lock-in (see ADR-0001)
- Fresh DB schema bootstrap migration
- Contract test harness: capture real Nextcloud traffic via mitmproxy → golden replay
- Stand up reference Nextcloud + capture rig
- Capture sprint: 50+ HAR files across desktop, iOS, Android flows
- WASM plugin host stub (logger-only ABI)

**Exit criteria**:
- `ncgo` binary boots and serves `/status.php`
- Migrations run cleanly on PostgreSQL, MySQL, SQLite
- OCS `cloud/capabilities` returns valid envelope
- ≥50 captured golden cases archived under `test/golden/`
- WASM hello-world plugin loads and logs
- CI matrix green on Linux/macOS × Go 1.27

See [`01-phase-0-blueprint.md`](01-phase-0-blueprint.md) for full detail.

### Phase 1 — Read-Only WebDAV + Core Auth (~4 months)

**Goal**: An existing Nextcloud desktop client can log in and browse files **read-only**.

In scope:
- `PROPFIND`, `GET`, `OPTIONS`, `HEAD` on `/remote.php/dav/files/{user}/`
- Login flow v2 (`/index.php/login/v2`) — desktop client OAuth-style flow
- App passwords (token issuance, revocation)
- Session cookies + Bearer tokens
- OCS `cloud/capabilities`, `cloud/user`
- Local filesystem storage backend
- Custom WebDAV properties: `oc:id`, `oc:fileid`, `oc:permissions`, `oc:size`,
  `oc:checksums`, `oc:owner-id`, `oc:owner-display-name`, plus `nc:` namespace
- ETag computation matching Nextcloud's algorithm
- Quota reporting (`d:quota-used-bytes`, `d:quota-available-bytes`)

Out of scope: writes, sharing, search, chunked upload, encryption, federation.

**Exit criteria**: Real desktop client connects, browses, downloads files. ≥80%
golden cases for read-only paths pass.

### Phase 2 — Read/Write WebDAV + Sharing Foundations (~5 months)

**Goal**: Desktop client performs full bidirectional sync. Public link sharing works.

In scope:
- `PUT`, `MKCOL`, `MOVE`, `COPY`, `DELETE`, `PROPPATCH`
- Chunked upload v2 protocol (resumable large uploads)
- Trash bin (`/remote.php/dav/trashbin/{user}/`)
- File versions (`/remote.php/dav/versions/{user}/`)
- Lock primitives (file locking API; full locking semantics are an ongoing risk)
- Public link sharing (read-only and read-write)
- OCS Sharing API v1/v2
- S3-compatible object storage backend (alongside local FS)
- Background job framework + scheduled jobs
- Server-side search (OCS Search API, filename + metadata)

Out of scope: federated sharing, encryption, calendar/contacts, talk, preview generation.

**Exit criteria**: Full bidirectional sync via desktop client; public links open in browser
and `curl`; ≥80% golden cases for write paths pass.

### Phase 3 — Calendar, Contacts, Federation (~6 months)

**Goal**: Replace bundled `dav` app's CalDAV/CardDAV; federation works between two
`nextcloud-go` instances and between `nextcloud-go` ↔ Nextcloud PHP.

In scope:
- CalDAV (RFC 4791): events, todos, scheduling, free-busy
- CardDAV (RFC 6352): address books, contacts, groups
- iCalendar/vCard parsing and serialization
- iOS and Android native calendar/contacts integration (via standard CalDAV/CardDAV)
- OCM (Open Cloud Mesh) federation — receive and send shares between instances
- Federated user search
- Notifications API (push to mobile via Nextcloud push proxy)
- Activity stream

Out of scope: encryption, Talk, Mail, Office.

**Exit criteria**: iOS/macOS Calendar.app and Contacts.app sync; cross-instance
federated share round-trip; ≥80% golden cases for CalDAV/CardDAV pass.

### Phase 4 — Plugin System, Admin, Migration Tool, Polish (~6 months)

**Goal**: Production-ready 1.0. Existing Nextcloud admins can migrate.

In scope:
- Full WASM plugin ABI implementation (see [`../specs/wasm-plugin-abi.md`](../specs/wasm-plugin-abi.md))
- Plugin packaging, signing (ed25519), install/upgrade UX
- Reference plugins: file tagger, simple webhook (both shipped in Phase 4d
  as `examples/`); OAuth provider **deferred** (host primitives exist; needs
  its own design pass — see Change Log 2026-09-22 Phase 4d)
- Admin web UI integration (existing Nextcloud Vue frontend, served by `ncgo`)
- `occ`-equivalent CLI (`ncgo-cli`) feature parity for operational commands
- `ncgo-cli import-nextcloud` — migrate from running PHP Nextcloud
  - Users, groups, app passwords, sessions
  - Files (via filecache scan + storage copy)
  - Shares (internal + public links)
  - Calendar/contacts data
- Encryption module (server-side encryption, E2EE pass-through)
- Preview generation (images via pure-Go libs; documents via external service)
- Talk, Mail, Office: **deferred to v2** — too large for v1 scope.

**Exit criteria**: 1.0 release. Documented production deployment. At least one
real-world migration completed from Nextcloud PHP to `nextcloud-go`.

## Effort & Team

| Phase | Calendar | Engineers | Eng-months |
|---|---|---|---|
| Phase 0 | 2.5 months | 2 | 5 |
| Phase 1 | 4 months | 3 | 12 |
| Phase 2 | 5 months | 3 | 15 |
| Phase 3 | 6 months | 3–4 | 21 |
| Phase 4 | 6 months | 4 | 24 |
| **Total** | **~24 months** | **3–4 avg** | **~77** |

Add ~10% slack for code review, ops, security audits → **~80–85 engineer-months**.

## Top Risks

| Risk | Phase | Mitigation |
|---|---|---|
| WebDAV custom property surface (`oc:`/`nc:`) larger than catalogued | Phase 1 | Enumerate from PHP source Week 2 of Phase 0 |
| `x/net/webdav` insufficient for Nextcloud extensions | Phase 1 | Fork from day 1; do not attempt upstream |
| Filecache + ETag propagation correctness | Phase 1–2 | Dedicated risk deep-dive before Phase 2 starts |
| Chunked upload v2 protocol edge cases | Phase 2 | Capture every chunked-upload variant in golden set |
| Lock semantics across distributed deployments | Phase 2 | Single-node only in v1; document as known limitation |
| TLS pinning blocks mobile traffic capture | Phase 0 | Fallbacks: Charles Proxy, sslsplit, decompiled mobile builds |
| Encryption module scope creep | Phase 4 | Fixed budget; defer SSE-C-style features if needed |
| Plugin SQL safety (multi-dialect parsing) | Phase 4 | Vitess parser for MySQL, custom shim for SQLite, pg_query_go for Postgres |

## Out of Scope for v1

- Talk (real-time communications)
- Mail
- Office (Collabora/OnlyOffice integration)
- Hosting the Nextcloud Vue frontend rewrite
- Loading unmodified PHP apps
- Multi-region active-active deployments

These become candidates for v2 (post-1.0).

## Change Log

- **2026-09-22** — Phase 4e3: `ncgo-cli import-nextcloud shares` imports
  internal (user/group) and public-link shares from a PHP Nextcloud database,
  resolving each share's path through `oc_filecache` + `oc_storages`
  (home storages only; external-storage shares skip with a warning)
  (ADR-0050). Link tokens import verbatim so existing `/s/<token>` URLs keep
  working; argon2id PHC link passwords import byte-identical and verify
  through ncgo's public-link check, while any other hash format (bcrypt etc.)
  skips the share rather than importing it unprotected. Remote/federated/
  circle shares (types 4/6/7) are skipped for later OCM re-creation;
  user/group shares get freshly generated tokens (Nextcloud stores none,
  ncgo requires one). Permission bitmasks are identical in both systems and
  import verbatim; stime→ms and UTC expiration→ms are converted, with
  unparseable expirations skipped (never silently extend validity).
  Preconditions (owner/recipient/target file present) skip with pointers to
  the earlier subcommands; idempotent on owner+path+type+recipient or token;
  share notes are dropped with one summary warning (no ncgo field). Dav (4e4)
  follows.
- **2026-09-22** — Phase 4e2: `ncgo-cli import-nextcloud files` imports user
  file trees from a PHP Nextcloud data directory (`--datadir`, `--user`
  repeatable, `--verbose`, inherited `--dry-run`) into the configured ncgo
  storage backend and filecache (ADR-0049). The datadir is walked directly —
  `oc_filecache` is only a cache, so the source database flags are not
  needed and the subcommand overrides the parent's source-DSN validation.
  Ingest goes through `DAV.Write`/`DAV.Mkdir` wired exactly as production
  wires them, so imports get checksums, etags, ancestor recalculation, and
  version snapshots on overwrite for free; source mtimes are preserved at
  second resolution. Skip-if-unchanged (same size + mtime, checked via the
  read-only filecache so `--dry-run` performs zero writes) makes re-runs
  cheap no-op scans and avoids spurious version snapshots. Only
  `<uid>/files/` is descended into — `files_trashbin`, `files_versions`,
  `uploads`, `cache`, `thumbnails`, `appdata_*`, and other non-user entries
  are excluded by construction; dotfiles import, symlinks and special files
  skip with warnings, and `files_encryption` users are skipped ("server-side
  encrypted source not supported"). Per-file errors warn and continue
  (best-effort, resumable). Unknown target users skip with a pointer to run
  `import-nextcloud users` first. Shares (4e3) and dav (4e4) follow.
- **2026-09-22** — Phase 4e1: `ncgo-cli import-nextcloud` scaffolding plus
  the `users` subcommand, importing users, groups, and group memberships
  directly from a PHP Nextcloud database (`oc_users`/`oc_groups`/
  `oc_group_user`, prefix configurable) into the configured ncgo DB
  (ADR-0048). Argon2id PHC hashes import verbatim (PHP `password_hash`
  format is what ncgo's verifier parses); other hash formats get a
  never-verifying sentinel plus a must-reset warning. Idempotent and
  resumable (existing rows skipped), `--dry-run` supported. Sessions and
  app passwords are deliberately NOT imported (source-secret-bound tokens);
  files, shares, and dav follow as 4e2–4e4 sibling subcommands.
- **2026-09-22** — Phase 4d2: `ncgo-cli` operational parity for the
  `user`, `group`, and `config` occ command families (ADR-0047). New
  commands: `user list|enable|disable|delete --yes|reset-password`,
  `group list|add|delete --yes|adduser|removeuser|members`, and
  `config get|set|delete` over the `appconfig` table (plugin config is
  appid `plugin`, key `<plugin_id>.<key>` — the v1 mechanism the Phase 4d
  webhook-forwarder README pointed at; that README's "CLI is a follow-up"
  gap is now closed). `users.SQLStore` gained `SetEnabled`, `Delete`,
  `List`, `GroupMembers`, `RemoveGroupMember`, `DeleteGroup`, and
  `ListGroups`. Notable decisions (full rationale in ADR-0047): deletes
  cascade to group memberships only — files/shares are NOT cascaded,
  matching occ's `user:delete` warnings; destructive commands require an
  explicit `--yes` (no TTY-prompt precedent in the repo); a dedicated
  `List` was added rather than reusing `Search` (which excludes disabled
  users and caps at 20); no auth changes were needed since disabled
  accounts are already rejected on password verify, bearer lookup, and
  session middleware. `cmd/ncgo-cli/cli_test.go` establishes the
  previously missing CLI test pattern (temp sqlite + minimal config file +
  `newRoot()` with captured output). Deferred occ families: maintenance
  mode toggle (needs an appconfig-backed runtime flag, not just a CLI
  writer), background-job inspection, app passwords, theming/branding.
- **2026-09-22** — Phase 4d: reference plugins shipped as buildable
  examples — `examples/file-tagger` (spec §10 walkthrough made real:
  `files.uploaded` subscriber that DDL-creates its `file_tags` table in
  `ncgo_on_install` and inserts `invoice` rows for `*.invoice.pdf`
  uploads) and `examples/webhook-forwarder` (forwards `files.uploaded`
  as a signed JSON POST using `config.*` for `webhook.url`/`webhook.secret`,
  `crypto_hmac` for `X-Signature`, and `http_request` under an exact
  `host[:port]` grant). Notable decisions, recorded here in lieu of an
  ADR (examples, not architecture): (1) the third planned reference
  plugin, the **OAuth provider, is DEFERRED** — the host primitives it
  would need (routes/OCS, db, http, config, crypto) now all exist, but it
  needs its own design pass rather than a rushed example; (2) the
  file-tagger writes `created_at = 0` because ABI v1 exposes no wall
  clock to freestanding guests (`ctx_deadline_unix_ms` is a deadline, not
  a clock) — a clock host function is a follow-up; (3) plugin config has
  no CLI yet, so the webhook-forwarder README documents the v1 mechanism
  (`appconfig` rows under `appid='plugin'` keyed
  `<plugin-id>.webhook.url`) and marks an `ncgo-cli` config command as
  follow-up; (4) guests decode the event payload with
  `github.com/vmihailenco/msgpack/v5` (already a host dependency), whose
  TinyGo compatibility is assumed-but-unverified in this environment —
  examples are validated via `ncgo-cli plugin check`, the `!tinygo` stub
  build, `go vet`, and lint, and a new optional `make example-plugins`
  target builds all three with TinyGo when available. No new tests: the
  host paths these examples exercise (event→db, event→http, config read)
  are covered by the 4c1–4c6 suites, and `cmd/ncgo-cli` has no `plugin
  check` test to mirror.
- **2026-09-22** — Phase 4c8: plugin WebDAV properties implemented —
  `webdav_register_prop` (hook-only, `webdav.props` grant) persists
  `prefix:local` name + getter/setter export names to a new
  `plugin_webdav_props` table (migration `0018`, uninstalled with the
  plugin); an empty setter registers a read-only prop. Live values are
  computed per request by a `plugins.PropProvider` attached to the files
  DAV as `webdav.LivePropProvider`: PROPFIND entries carry
  `Entry.ExtraProps` emitted as `<x:NAME xmlns:x="NS">value</x:NAME>` under
  the per-plugin namespace URI `http://ncgo.local/ns/plugin/<plugin_id>`
  (same local name across plugins never collides), and PROPPATCH offers
  each op to the provider first (403 read-only/detached, 500 guest failure,
  remove = set-empty) before falling through to the persisted
  `oc:favorite` logic. Getter/setter use a raw-string convention
  (getter writes the value into a host-allocated 4 KiB buffer and returns
  the byte count) — a documented deviation from the spec §7 MessagePack
  sketch; getter failures omit the prop and never fail the PROPFIND (see
  ADR-0046).
- **2026-09-22** — Phase 4c7: plugin jobs implemented — `job_enqueue`
  validates the plugin-local name (1–128 bytes of `[A-Za-z0-9_.-]`, else -2),
  checks `jobs.register` (-3), requires a configured runner (-12), and
  rejects plugins without an `on_job` entry point (-2); `run_at_unix_ms` ≤ 0
  or past means now, more than ten years out is -2. Rows are stored under
  one adapter job per plugin (`plugin.<plugin_id>`) carrying a MessagePack
  `{name, payload}` envelope; the adapter drops undeliverable work (detached
  plugin, poison envelope, no entry point) and retries guest failures
  (non-zero i32 or trap) through the jobs runner, delivering the
  plugin-local name verbatim to `ncgo_on_job`. Registration happens in
  `startOne` (duplicates tolerated, failures logged, boot never fails);
  HostConfig gains Jobs; pluginsdk gains JobEnqueue/JobArgs (see ADR-0045).
- **2026-09-22** — Phase 4c6: `config.*` host functions implemented — a new
  generic `appconfig` store (migration `0017_appconfig`, all three
  dialects; the Nextcloud `oc_appconfig` analogue) backs `config_get` /
  `config_set` under `appid = "plugin"` with keys namespaced
  `<plugin_id>.<key>`; `config.read` / `config.write` grants are per-key
  globs matched against the plugin-local key (-3), empty keys are -2,
  missing keys -4, a nil store -12, and values are capped at 64KiB (-11,
  get with a too-small buffer also -11). HostConfig gains AppConfig;
  pluginsdk gains ConfigGet/ConfigSet (see ADR-0044).
- **2026-09-22** — Phase 4c5: outbound `http.*` host functions implemented —
  `http_request` validates method/scheme and enforces the `http.outbound`
  allowlist with default-port normalization (grant `example.com` covers
  default ports only, `example.com:8080` matches exactly, no subdomain
  implication, case-insensitive), every redirect target is re-validated on
  a shallow client copy (non-granted → -3), timeouts default to 10s and
  clamp to 30s (deadline → -6), plugin-supplied `Host` headers are dropped,
  and responses stream through per-instance handles on the 16-response
  budget with closeAll cleanup (status / header joined with `", `", absent
  header → 0 bytes / body_read EOF → 0 / close; stale handles → -4).
  HostConfig gains HTTPClient (nil = default 30s client); pluginsdk gains
  the HTTPDo + HTTPResponse bindings (see ADR-0043).
- **2026-09-22** — Phase 4c4: `storage.*` host functions implemented —
  scheme-prefix path routing (`user:` = calling user's files via the files
  DAV, `system:` = per-plugin namespace under
  `appdata_<instanceID>/plugins/<plugin_id>/` on the default storage
  backend, bare paths default to `user:`), granular per-scope
  `storage.read`/`storage.write` capability checks, streaming read/write
  handles on the 64-stream budget with closeAll cleanup, user writes
  spooled to temp files and committed one-shot through `DAV.Write` on
  stream close (1 GiB cap, trash-on-delete via `DAV.Remove`, rename via
  `DAV.Move`), and `events.Event.UserID` propagation so event-driven
  plugin calls get the uploading user's identity (explicit call metadata
  on the publish ctx wins). HostConfig gains
  Files/SystemStorage/SystemPrefix/MaxSpoolBytes; pluginsdk gains the
  storage bindings (see ADR-0042).
- **2026-09-22** — Phase 4c3: plugin HTTP route + OCS endpoint registration
  and dispatch — `route_register`/`ocs_register` implemented (hook-only,
  capability + `/apps/<plugin_id>/` namespace enforcement, method/handler
  validation), routes persisted in the new `plugin_routes` table (migration
  0016, upsert, deleted on uninstall), boot-time mounting via
  `plugins.MountRoutes` (plain routes with DAV auth, OCS under
  `/ocs/v1.php`+`/ocs/v2.php` with `ocs.Auth`), and request dispatch through
  the `ncgo_on_request`/`ncgo_response_*` export family with streaming
  bodies and OCS envelope wrapping. Request bodies travel inline as
  msgpack `body_bytes` (≤ 1 MiB) instead of spec §7 `body_handle` (see
  ADR-0041).
- **2026-09-22** — Phase 4c2: in-process event bus (`internal/events`,
  synchronous fan-out with re-entrant snapshot semantics) and plugin event
  delivery — `event_publish` implemented with `events.publish` glob +
  `core.*` reservation, manifest `events.subscribe` + `ncgo_on_event`
  delivery via guest alloc/write/call/free, publisher self-skip, delivery
  failures logged and never propagated, and the first core emission
  (`files.uploaded` with MessagePack `{user, path, size, created}`) from the
  files DAV (see ADR-0040).
- **2026-09-21** — Phase 4c1: `db.*` host functions implemented — SQL
  safety via xwb1989/sqlparser AST table extraction (fail closed),
  SELECT/write/DDL classes with DDL restricted to lifecycle hooks,
  MessagePack rows, transactions, and handle cleanup on every release
  path (see ADR-0039). Resolves spec §14 Q1 for v1.
- **2026-09-21** — Phase 4b: `.ncplugin` archives, ed25519 signing with
  operator-pinned trusted keys, `0015_plugins` registry, verified
  install/upgrade (capability re-approval) / uninstall, boot-time loading
  of enabled plugins, and the full `ncgo-cli plugin` command set
  (see ADR-0038).
- **2026-09-21** — Phase 4a: plugin runtime core — typed capabilities with
  default-deny checks, per_request/pooled/singleton instance manager with
  per-instance handle tables, full ncgo-abi/1 host function surface
  registered (log/ctx/crypto/cache implemented; db/storage/http/events/jobs/
  routes/ocs/webdav/config return ErrUnsupported until their increments),
  pluginsdk bindings (see ADR-0037).
- **2026-09-21** — Phase 3f3: OCS sharees `/recommended` returns
  distinct local user/group recipients of the caller's current shares,
  most recent first (see ADR-0036).
- **2026-09-21** — Phase 3f2: CardDAV contact groups proven — KIND:group
  vCards with MEMBER round-trip as plain VCARD objects; no
  group-specific server handling needed (see ADR-0035).
- **2026-09-21** — Phase 3f1: OCS sharees local user/group typeahead
  via `users.Search`/`SearchGroups`; exact uid/gid lands in exact.*,
  other matches in users/groups (see ADR-0034). sharees/recommended
  remains later.
- **2026-09-21** — Phase 3e4: local iTIP scheduling — organizer writes
  deliver invites to local attendees' default calendars, attendee
  PARTSTAT replies update the organizer copy, organizer deletes cancel
  attendee copies; principals emit `calendar-user-address-set` (see
  ADR-0033). iMIP, inbox/outbox, and SEQUENCE remain later. Phase 3
  CalDAV scope complete.
- **2026-09-21** — Phase 3e3: calendar sharing via `calendar_shares`
  and POST `cs:share`; shared calendars appear as
  `{uri}_shared_by_{owner}` in the sharee's home with `share-access`
  and `nc:owner-principal`; read-write enforced for object writes (see
  ADR-0032). Invite accept flow, group shares, and scheduling remain
  later.
- **2026-09-21** — Phase 3e2: `free-busy-query` REPORT returns a raw
  VFREEBUSY document via new `webdav.RawReportFS`; RRULE expansion
  (FREQ/INTERVAL/COUNT/UNTIL, cap 1000), TRANSP:TRANSPARENT excluded,
  busy intervals merged (see ADR-0031). BY* rules, scheduling, and
  calendar sharing remain later.
- **2026-09-21** — Phase 3e1: CalDAV VTODO objects (DTSTART/DUE/DURATION/
  COMPLETED/CREATED anchors), `calendar-query` comp-filter component
  filtering, and VEVENT+VTODO `supported-calendar-component-set` (see
  ADR-0030). Free-busy, scheduling, and calendar sharing remain later.
- **2026-09-21** — Phase 3d8: OCS sharees `lookup=true` queries the
  configured lookup server (default `https://lookup.nextcloud.com`,
  `sharing.lookup_server`) and returns hits in the `lookup` collection;
  failures degrade to empty (see ADR-0029). CalDAV leftovers remain
  later.
- **2026-09-21** — Phase 3d7: outbound federated share delete posts
  `SHARE_UNSHARED` to `{endPoint}/notifications`; inbound
  `POST /ocm/notifications` drops the matching `ocm_incoming` row (see
  ADR-0028). Lookup server and CalDAV leftovers remain later.
- **2026-09-21** — Phase 3d6: inbound federated folder Depth 1 PROPFIND,
  nested GET, and permission-gated PUT/DELETE/MKCOL proxy (see
  ADR-0027). Unshare notify, lookup server, and CalDAV leftovers remain
  later.
- **2026-09-21** — Phase 3d5: OCS sharees returns `exact.remotes` for a
  cloud ID; `sharee.query_lookup_default` is false (see ADR-0026).
  Folder/write proxy, unshare notify, and lookup server remain later.
- **2026-09-20** — Phase 3d4: `files_sharing.federation` is an object with
  `outgoing`/`incoming` true so desktop can discover federated share
  (see ADR-0025). Federated search, folder/write proxy, and unshare remain
  later.
- **2026-09-20** — Phase 3d3: inbound federated file DAV GET/HEAD proxies
  `{origin}/public.php/webdav/` with Basic token (see ADR-0024). Federation
  capability, folder tree, and write proxy remain later.
- **2026-09-19** — Phase 3d2: outbound OCS `shareType=6` federated shares,
  remote OCM discovery and `POST {endPoint}/shares`, rollback on notify
  failure (see ADR-0023). Federation capability and inbound DAV proxy remain
  later.
- **2026-09-19** — Phase 3d: inbound OCM discovery (`/.well-known/ocm`,
  `/ocm-provider`), `POST /ocm/shares`, OCS `remote_shares`, DAV metadata
  mount with GET 501 (see ADR-0022). Outbound federation remains later.
- **2026-09-19** — Phase 3c: Notifications and Activity OCS list/get/delete
  on `/ocs/v{1,2}.php/apps/notifications/api/v2/notifications` and
  `/ocs/v{1,2}.php/apps/activity/api/v2/activity`, SQL seed rows, no push
  (see ADR-0021). OCM remains later.
- **2026-09-19** — Phase 3b: CardDAV address books (RFC 6352) on
  `/remote.php/dav/addressbooks/users/`, default `contacts` book,
  `REPORT` addressbook-query/multiget, principals `addressbook-home-set`
  (see ADR-0020). Notifications and OCM remain later.
- **2026-09-19** — Phase 3a: CalDAV events (RFC 4791) on
  `/remote.php/dav/calendars/`, principals, `REPORT` calendar-query/multiget,
  default `personal` calendar (see ADR-0019). CardDAV, notifications, and OCM
  remain later.
- **2026-09-19** — Phase 2e2: files_lock OCS by fileid, Depth infinity
  ancestor CheckLock, shared lock remains 403 (see ADR-0018).
- **2026-09-19** — Phase 2k: user/group shares (`shareType` 0/1),
  `0009_share_recipients`, recipient files-jail listing (see ADR-0017).
  Lock leftovers remain later.
- **2026-09-19** — Phase 2i: OCS unified-search `files` provider, basename
  `SearchByName` over filecache (see ADR-0016). User/group shares and lock
  leftovers remain later.
- **2026-09-19** — Phase 2g: S3-compatible `storage.Storage` via minio-go,
  `openStorage` `type == "s3"` (see ADR-0015). Search and user/group shares
  remain later.
- **2026-09-19** — Phase 2h: `jobs.SQLStore` / `SQLRunner`, `shares.expire` and
  `locks.expire`, App Start/Stop (see ADR-0014). S3, search, and user/group
  shares remain later.
- **2026-09-19** — Phase 2f: public-link sharing (`shareType=3`), `0008_shares`,
  OCS files_sharing CRUD, `/public.php/webdav`, and `GET /s/{token}`
  (see ADR-0013). User/group shares, S3, jobs, and search remain later.
- **2026-09-19** — Phase 2e: RFC 4918 exclusive write LOCK/UNLOCK on files DAV,
  `0007_file_locks`, 423 on unlocked writes, DAV class 2 (see ADR-0012).
  Sharing, S3, jobs, and search remain later.
- **2026-09-18** — Phase 2d: WebDAV PROPPATCH for `oc:favorite` on
  path-keyed `file_properties`, files PROPFIND emits favorite
  (see ADR-0011). LOCK, sharing, S3, jobs, and search remain later.
- **2026-09-18** — Phase 2c: file versions on `/remote.php/dav/versions/`,
  overwrite snapshots, restore MOVE, `files.versioning` (see ADR-0010).
  PROPPATCH, LOCK, sharing, S3, jobs, and search remain later.
- **2026-09-18** — Phase 2b: trashbin on `/remote.php/dav/trashbin/`, DELETE
  into `trash_items`, restore MOVE, `files.undelete` (see ADR-0009). Versions,
  sharing, S3, jobs, and search remain later.
- **2026-09-18** — Phase 2a: filecache write goldens, `0003_uploads`, chunked
  upload v2 on `/remote.php/dav/uploads/`, and `dav.chunking` / `files.chunked_upload`
  capabilities (see ADR-0008). Trash, sharing, S3, jobs, and search remain later.
- **2026-09-18** — Phase 1 code path: SQL filecache, localfs DAV, webdav-root
  alias, Bearer/session/bruteforce middleware, and in-process read-only WebDAV
  goldens (see ADR-0007). Desktop client smoke and HAR capture remain operator
  work.
- **2026-09-17** — CI matrix updated to Linux/macOS × Go 1.27 (see ADR-0006).
- **2026-04-29** — Initial plan committed.
