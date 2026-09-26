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

- **2026-09-26** — Phase 5w-4a: SSE password-wrapped user keys —
  **enrollment + identity path** (ADR-0101), delivering ADR-0100's phase
  4-a and recording two implementation refinements (strike-annotated
  there): the session-copy blob gains a key-ID byte (O(1) ring selection
  after master-key rotation), and the FK-availability corner — recipient
  wraps need no session (public keys) but the FK source does for enrolled
  owners, so the write path threads the plaintext FK from `Allocate`
  (`storage.FileKeyWriter` → `dav.write` → `KeySharer` →
  `WrapKeyForFK`/`ReWrapShareesFK`); third-party-ctx hooks fail
  best-effort and heal on the next authorized write (documented residual
  gap). Migration 0021 (three dialects: `user_key_pw`,
  `file_keys.scheme`, `sessions.sealed_uk`), `pwbox.go` constructions
  byte-exact per ADR-0100 §3 (argon2id KEK, 60 B pw_sealed_uk, 92 B
  X25519 box, `m=…,t=…,p=…` KDF params), the four-branch
  enrollment/unenrollment state machine at password login (crash-resumable
  in both directions, enroll-race unique-violation resume), the
  Resolve/Allocate identity path (enrolled owners resolve through the
  READING user's wrap row; `ErrKeyLocked` strictly distinct from
  `ErrIntegrity`/`ErrUnresolvableKey`), session/principal plumbing
  (`sessions.sealed_uk` → `Principal.UnlockedKey` via the fail-closed
  `SessionKeyUnlocker` seam; basic-only `LoginKeyHandler` on browser login
  and login-v2 grant; loud 500 + session delete on seal/store failure),
  reset-password refusal + `--force` destruction, sweep locked-skip
  accounting, status enrollment counts, and the dual-matched 403 mapping
  (`internal/files/keylocked.go`; webdav stays encrypt-free). Phase 4-a
  breaks app-password DAV for enrolled users until 4-b — opt-in,
  documented. Zero new dependencies.
- **2026-09-26** — Phase 5w-4 **design**: password-wrapped user keys
  (ADR-0100), supplying the phase-4 design ADR-0096 deferred (its two
  forward references annotated). The threat-model pivot: an enrolled
  user's master-sealed UK is DELETED after re-wrapping every wrap row, so
  an at-rest server compromise exposes nothing for users without an
  active session. ADR-0096's symmetric-only decision is revisited under
  new evidence — the lock problem (share grants, incoming-share writes,
  and group adds must wrap FOR offline users) makes per-user X25519
  keypairs necessary: the public key wraps for locked users, the private
  key lives only password-sealed (`argon2id` KEK, already in-tree — zero
  new dependencies), master-sealed on live session rows, and (phase 4-b)
  HKDF-sealed under app-password tokens. Enrollment is lazy at password
  login behind `encryption.password_wrapped_keys`; unenrollment mirrors
  at password login with the flag off. Reads resolve through the READING
  user's own wrap row (the ADR-0098 recipient rows assume their pinned
  phase-4 role) with a new `ErrKeyLocked` sentinel → 403; public links,
  background readers, and CLI sweeps skip/fail loudly on enrolled users'
  files; `reset-password` refuses enrolled users without `--force`
  (documented data loss). Migration 0021 (three dialects): `user_key_pw`,
  `file_keys.scheme`, `sessions.sealed_uk` (+ `app_token_keys` in 4-b).
  Implementation phased 4-a (enrollment + identity read/write path) and
  4-b (token wraps); trade-off table in the ADR.
- **2026-09-26** — Phase 5w-3: SSE per-user keys **lifecycle + tooling**
  (ADR-0099), delivering ADR-0096's phase 3 and resolving the phase-3
  deferrals in ADR-0097/0098. The `users.SQLStore.UserKeys` hook (structural,
  mirroring ADR-0098's `MemberKeys`) mints user keys eagerly at account
  creation and purges a deleted user's key rows (fires between membership
  cleanup and the row delete — the uid must still resolve; best-effort
  Warn-logged, wired in app, `ncgo-cli user add/delete`, and
  `import-nextcloud users`; the bootstrap admin stays lazy-minted, a
  documented gap reconcile backfills). New concrete-only resolver methods:
  `OnUserCreated`/`OnUserDeleted` (the user's own rows only — files of a
  deleted owner become unresolvable, spelled out in the delete help),
  `ResealUserKeys` (rotation hygiene, aborts `ErrUnresolvableKey` naming the
  user on an out-of-ring key id), `PruneStaleKeys`, `MintMissingUserKeys`,
  and the `KeyInventory` health read model (nine dialect-agnostic counts —
  no schema changes). `encryption status` prints the per-user inventory and
  fails non-zero on broken v3 files (owner wrap missing = UNREADABLE);
  `rotate-keys` re-seals UKs under the current key id after a successful
  rotation; new `encryption reconcile [--dry-run]` mints missing UKs, wraps
  every existing share's recipients, and prunes stale rows — all idempotent,
  non-zero exit on wrap errors. Hooks stay best-effort because key rows are
  the phase-4 substrate, never a read-path dependency; FK constraints stay
  omitted (explicit, tested purges over connection-dependent cascades). Zero
  new dependencies.
- **2026-09-26** — Phase 5w-2: SSE per-user keys **share wrap/revoke**
  (ADR-0098), delivering ADR-0096's phase 2. Resolver wrap API
  (`WrapKeyFor`/`UnwrapKeyFor`/`ReWrapSharees` — idempotent wraps, the
  owner guard that never orphans a file from its owner, overwrite carry
  that aborts before deleting old rows on failure), the `files.KeySharer`
  service over narrow structural seams (no new import edges) with hooks on
  share create/delete, lazy + background expiry (`ListExpired` + unwrap of
  exactly the reaped shares), the DAV write path (`WrapForWrite` for
  covering shares — target-is-path-or-ancestor via a dialect-neutral
  ancestor `IN` list — plus `ReWrapForOverwrite`), and group membership
  add/remove (`users.SQLStore.MemberKeys`, wired in app and
  `ncgo-cli group`). Wrap rows belong to the live file: overwrites carry
  recipients to the fresh key and delete all superseded rows (version
  snapshots re-seal under their own UUIDs and keep resolving — amends
  ADR-0097's persist note). Every hook is best-effort Warn-logged (recipient
  rows are the phase-4 substrate, never a read-path dependency); link/OCM
  shares and import/bootstrap membership are documented skips; the
  write/unshare race invariant (recipient row iff share exists) is closed
  by a reconciliation re-read and pinned under `-race`. Zero new
  dependencies.
- **2026-09-26** — Phase 5w-1: SSE per-user keys **envelope + resolver**
  (ADR-0097), delivering ADR-0096's phase 1. v3 header
  (`"NCGOENC3" || keyUUID(16) || salt(32)`, 56B; chunk layer verbatim,
  data key `HMAC-SHA256(FK, salt)`), the `KeyResolver` seam with a SQL
  implementation (lazy per-user UK sealed under the current ring position
  with `"NCGOUK1"||be64(user_id)` AD; per-file FK wrapped with
  `"NCGOFK1"||keyUUID||be64(user_id)` AD; owner-first then wrap-row
  fallback resolution; `ErrUnresolvableKey` never `ErrIntegrity`), schema
  migration 0020 (`user_keys`, `file_keys`, nullable `files.key_uuid`) in
  all three dialects, the `storage.KeyUUIDWriter` seam feeding
  `files.key_uuid` on DAV writes, opt-in `encryption.per_user_keys` config
  (validated against `enabled`), `openStorage` gaining the DB handle in app
  and CLI, sweep direction four (`SweepRekeyV3` + `ncgo-cli encryption
  rekey-v3`) for eager migration, and `SweepRotate` skipping v3. Nil
  resolver ⇒ bit-identical v1/v2 behavior (rollback carve-out preserved);
  zero new dependencies.
- **2026-09-26** — Phase 5w: SSE per-user keys **design** (ADR-0096),
  closing ADR-0074's deferral with the missing per-recipient wrapping +
  admin-recovery design. Key hierarchy: random per-file key (v3 envelope
  header `"NCGOENC3" || keyUUID || salt`), per-user keys sealed under the
  master key, a `file_keys` wrap table with AEAD-bound wraps, a narrow
  `KeyResolver` seam at the storage decorator (nil = today's behavior,
  bit-compatible), share-grant wrap / revoke-unwrap integration, and
  master-key recovery built into the chain. Implementation is phased
  (1 envelope+resolver, 2 sharing, 3 lifecycle+CLI, 4 password-wrapped user
  keys — the only phase that changes the threat model, with its own ADR);
  no code in this increment.
- **2026-09-26** — Phase 5v: per-plugin memory introspection (ADR-0095). The
  two long-deferred memory items (ADR-0055's high-water carve-out, ADR-0091's
  out-of-scope note, the spec's "validated but not applied"
  `runtime.memory_limit_mb`) rested on a false premise — wazero's
  `api.Module.Memory().Size()` always reported live linear-memory bytes; only
  mid-call grow hooks and per-module limits are absent. The observability
  registry gains a gauge kind (`RegisterGauge`/`SetGauge`/`SetGaugeMax`,
  `# TYPE gauge` render, `GaugeSeries` read model) with two new families:
  `ncgo_plugin_memory_high_water_bytes{plugin}` and
  `ncgo_plugin_memory_limit_exceeded_total{plugin}`. The plugin instance
  manager measures memory at every release/close and soft-enforces
  `memory_limit_mb` retrospectively — an over-limit instance is destroyed
  like a trapped one (pool replenished, singleton dropped), the completed
  call's result untouched; the in-call ceiling stays the host-global 256 MiB
  clamp. Per-plugin wazero runtimes for mid-call enforcement recorded as
  considered/rejected. The console plugins panel shows memory high-water and
  limit breaches; wasmgen gains `MemGrowModule`.
- **2026-09-26** — Phase 5u: atomic conditional DAV writes (ADR-0094). PUT
  evaluated If-Match/If-None-Match after a Stat and plain PUT ignored the
  `If:` etag state list NC desktop sends — both TOCTOU lost-update windows;
  chunked assemble re-checked the If-etag the same way. New
  `webdav.WriteCond` (IfETags/IfMatch/IfNoneMatch + `Evaluate`) and the
  optional `webdav.CondWriteFS` interface let filesystems enforce
  preconditions atomically: `files.DAV` serializes same-path writes through
  a 256-stripe FNV-1a lock table shared by `Write` and `WriteIf` (plain
  writes serialize too — plugin/CLI writers race the same storage keys),
  evaluates the cond against the in-lock filecache state, and CAS-guards the
  overwrite with `SQLStore.UpdateMetaIfETag`
  (`UPDATE ... WHERE id=? AND etag=?`, zero rows → `ErrETagConflict` → 412).
  `If:` parsing handles uri-tagged and multiple lists, strips weak `W/`
  tags, skips `Not` lists; lock-token-only `If:` stays on the fast path,
  which also drops the always-on pre-write Stat. Assemble now delegates the
  If-etag to `WriteIf` (409 overwrite semantics kept), PublicDAV mirrors
  with `WriteIf`, version restores serialize per path, and OCM-remote
  mounts pass conditions through unenforced (documented). Multi-process
  content clobber (staging-key publish) listed as follow-up.
- **2026-09-25** — Phase 5t: plugin stdout/stderr routed into the host log
  stream (ADR-0093), closing the ADR-0092 follow-up. Each instance's WASI
  stdio fds are wired to a per-instance line-buffering writer on the module
  config (`internal/plugins/stdiolog.go`): stdout→INFO, stderr→WARN,
  `plugin.id`/`plugin.version`/`plugin.stdio` attrs, 4096-byte line cap with
  truncation marker, blank lines dropped, `\r` stripped, partial lines
  flushed at instance close, and `fd_write` can never fail the guest.
  wasmgen gains `WASIFdWriteModule` (real `fd_write` calls plus the required
  export stubs); ABI spec §7 updated.
- **2026-09-25** — Phase 5s: TinyGo toolchain landed + plugin target
  migration to wasip1 reactor (ADR-0092). With TinyGo 0.42.0 finally
  installed, building the example plugins exposed four latent defects the
  wasmgen-probe-only posture could never see: pluginsdk's `wasm` build
  tag/`_wasm.go` suffix were never selected under TinyGo's wasm-unknown
  (GOARCH=arm) so historical artifacts were hollow stub builds; the ctx
  bindings passed wasmimport functions as values (TinyGo rejects); msgpack
  pulls a `go` statement that wasm-unknown's scheduler=none refuses; and
  wasm-unknown+asyncify is unrunnable here because its task scheduler needs
  host stack-switching hooks wazero cannot express. Decision: plugins build
  with `-target=wasip1 -buildmode=c-shared` (self-contained asyncify,
  reactor `_initialize` per instance), the host instantiates wazero's WASI
  and load-gates it to exactly six imports (fd_write, poll_oneoff,
  clock_time_get, args_sizes_get, args_get, random_get — no stdin/fs/
  sockets), and pluginsdk's 17 binding files moved to `tinygo`/`_tinygo.go`.
  New tinygo-gated e2e test compiles and runs examples/hello-plugin for
  real; the WASI policy probes now pin path_open rejected / fd_write
  allowed. Zero new dependencies (WASI ships inside wazero).
- **2026-09-25** — Docs housekeeping: struck stale follow-up pointers whose
  work shipped under later ADRs — ADR-0056's cache-key cleanup
  (ADR-0058+0063) and §13 private-IP egress check (ADR-0057), ADR-0052's
  SSE-C bullet (rejected by ADR-0074), ADR-0071's tokens-subcommand
  pointers (shipped by ADR-0073), and ADR-0080's precompressed-assets
  pointer (ADR-0081). No code changes; no new decisions.
- **2026-09-25** — Phase 5r: per-plugin metrics panel in the admin console
  (ADR-0091), closing the ADR-0055 dashboard deferral (except memory
  high-water, still blocked on wazero introspection). The render-only
  registry gains a typed read API — `CounterSeries`/`HistogramSeries`
  snapshots (canonical labels, cumulative buckets ending +Inf, defensive
  copies) plus `Quantile` with Prometheus `histogram_quantile` interpolation
  semantics — behind `GET /console/api/plugins`, which aggregates the six
  per-plugin families into one id-sorted row per plugin: call/error counts
  summed over function/result/op/scope, latency mean/p50/p95/p99 from the
  per-function (per-entry) histogram series merged bucket-by-bucket over the
  single fixed le grid, storage bytes, and capability denials. A nil
  registry (metrics disabled) answers 503, which the console's new Plugins
  section renders as a section-body "metrics disabled" message instead of
  the table. Zero new dependencies.
- **2026-09-25** — Phase 5q: admin-console surfacing of notifications
  (ADR-0090), closing the ADR-0082 deferral that left notifications visible
  only to their recipient through the OCS bell. `notifications.SQLStore`
  gains `ListRecent(ctx, limit)` — newest-first across all users, reading the
  denormalized `user_uid` column with no users-table join — behind a narrow
  `console.NotifsStore` dependency, a read-only `GET
  /console/api/notifications` endpoint (id, user, app, object type/id,
  subject, message, link, icon, RFC3339 created_at; rich templates stay out
  of v1), and a Notifications section in the no-framework console shell
  following the jobs view's fetch/table/refresh pattern.
- **2026-09-25** — Phase 5p: plugin old-version archive GC (ADR-0089),
  closing the ADR-0056 deferral that kept every superseded
  `<install_dir>/<id>/<version>.ncplugin` forever. After a successful
  upgrade install (post-Upsert only, so failed upgrades never lose the
  still-installed old archive), a best-effort sweep keeps exactly the
  current and previous archives — the minimal rollback story — and deletes
  anything strictly older; ReadDir/Remove failures Warn-log and never fail
  the install. Fresh installs do not sweep, one plugin's GC never touches
  another's directory, Uninstall still RemoveAlls the whole tree, and a
  same-version reinstall is a GC no-op so the previous distinct version's
  archive — the last rollback artifact — survives a routine re-upload,
  while a downgrade keeps both directions' archives. The directory is now
  bounded at two files per plugin.
- **2026-09-25** — Phase 5o: guest entry-point call metrics (ADR-0088),
  closing the ADR-0055 deferral that left only host calls instrumented.
  `Plugin.call` — the single funnel for `Call`, `callEntry`, and the
  lifecycle hooks — now records `ncgo_plugin_entry_calls_total`
  `{plugin,entry,result}` and `ncgo_plugin_entry_call_duration_seconds`
  `{plugin,entry}` whenever the host carries a metrics registry; a nil
  registry keeps the call path byte-identical (zero added overhead). Result
  classes are `ok`/`timeout`/`trap`/`missing_export`/`error`, classified on
  the Go error with `context.DeadlineExceeded` checked before `ErrTrap`
  because `wrapTrap`'s `%w: %w` double-wrap matches both; the `entry` label
  space is bounded by the plugin manifest, not network input.
- **2026-09-24** — Phase 5n: WebP preview input (ADR-0087), closing the
  input half of ADR-0053's WebP follow-up. A blank import of
  `golang.org/x/image/webp` (already in the approved x/image module — no
  new dependency) registers VP8/VP8L decoding; `image/webp` joins the
  content-sniff whitelist in both the serve path and the pregeneration
  head-sniff. Output re-encodes as PNG (no WebP encoder in scope), so
  VP8L alpha survives; animated WebP fails Decode and takes the uniform
  404 like any unpreviewable content. Fixtures generated with PIL are
  checked in under internal/preview/testdata/.
- **2026-09-24** — Phase 5m: preview fill mode (ADR-0086), closing
  ADR-0053's fill/crop follow-up for `mode=fill`. `mode=fill` now renders
  an aspect-fill, centre-cropped preview (cover-scale with
  `min(max(x/w, y/h), 1.0)`, crop to `min(dw,x) x min(dh,y)`, never
  upscaling); any other `mode` value — including offset `crop` — falls
  back to the v1 aspect-preserving fit with no new 400s. The cache key
  appends a `"\nfill"` marker for fill renders while fit keys stay
  byte-identical to the pre-fill format, so existing cache entries remain
  valid and fit/fill variants coexist (singleflight separation comes free
  with the key). Pregeneration (ADR-0084) deliberately warms fit boxes
  only.
- **2026-09-24** — Phase 5l: preview cache garbage collection (ADR-0085),
  closing ADR-0053's cache-GC follow-up (also carried by ADR-0084). Cache
  keys are one-way content-derived hashes, so orphaned etag-keyed entries
  cannot be detected exactly; the new periodic `preview.gc` job instead
  sweeps the flat cache prefix by modtime TTL (`previews.cache_max_age`,
  default 720h, validated >= 1h or 0 for the default) — safe because the
  worst case is one regeneration of a hot entry, never wrong bytes. The job
  throttles itself to one pass per 24h in-memory against the runner's 5s
  periodic re-enqueue, tolerates per-entry delete failures (Warn + continue,
  since the runner retries a failed Run forever), treats a missing cache
  directory as a completed empty pass, and is registered only when
  `previews.enabled` — before `jr.Start`, which seeds periodic jobs only
  for already-registered names.
- **2026-09-24** — Phase 5k: preview pre-generation on upload events
  (ADR-0084), closing ADR-0053's pre-generation follow-up. With
  `previews.pregenerate_enabled` (opt-in, default false) every
  `files.uploaded` event enqueues one `preview.pregenerate` jobs row
  carrying the msgpack payload verbatim; the job renders the configured
  hot sizes (`previews.pregenerate_sizes`, default `[32, 256]` — files
  list and gallery boxes, clamped to `max_dimension` and deduped) into the
  etag-keyed cache ahead of any client request. The background path
  head-sniffs 512 bytes before the full read so non-images never trigger a
  256 MiB `ReadAll`, shares the serve path's `render` helper and
  singleflight group (HTTP behavior byte-identical), and is idempotent
  under at-least-once delivery; per-file conditions return nil while only
  infrastructure failures retry.
- **2026-09-24** — Phase 5j2: background share-expiry dismisses
  notifications (ADR-0083), closing the one deletion path ADR-0082 did
  not cover. The `shares.expire` job bulk-deletes by predicate without
  going through `Service.Delete`, so a share reaped there before any
  read left its bell rows behind. `ShareStore.DeleteExpired` now returns
  the deleted ids (select-then-delete in one transaction — the returned
  ids name exactly the removed rows), and the job dismisses each
  `("share", "ocinternal:<id>")` best-effort through the 5j
  `ShareNotifier` interface, wired from the always-constructed
  `notifStore`. All three share-deletion paths now dismiss.
- **2026-09-24** — Phase 5j: file-share notifications (ADR-0082), closing
  the ADR-0032 invite-notifications follow-up with the verified verdict.
  Creating a USER or GROUP file share now drops an NC-shaped bell into the
  sharee's OCS notifications: app `files_sharing`, object
  `share`/`ocinternal:<id>` (NC's providerId:id), the `incoming_user_share`
  / `incoming_group_share` subjects and rich templates with
  `highlight`/`user`/`user-group` parameters, `should_notify` set, no
  actions (ncgo auto-accepts; NC's accept/reject exist only for pending
  shares). Group shares fan out to every member except the actor. Unshare
  and expiry eagerly delete the bells of all recipients via the new
  `notifications.SQLStore.DeleteByObject` — where NC filters dead-share
  rows lazily at render, ncgo deletes so polling clients stop seeing ETag
  churn. A notification failure never fails a share: the sink sits behind
  the `sharing.ShareNotifier` interface and errors are Warn-logged. DAV
  (calendar/addressbook) shares send nothing, exactly like Nextcloud's
  `apps/dav`; an invite state machine there would contradict the verified
  no-invite-state `oc_dav_shares` semantics (ADR-0071). Deferred:
  group-join backfill, remote-share subjects, activity-stream entries,
  admin-console surfacing.
- **2026-09-23** — Phase 5i: precompressed static assets (ADR-0081),
  resolving the ADR-0054 follow-up. `StaticUI.serveFile` now negotiates
  `.br`/`.gz` sidecars: brotli over gzip on a fixed server preference,
  explicit tokens only (`q=0` excludes, wildcards ignored — the nginx
  `gzip_static` stance), sidecars must exist as regular files contained
  in the root (escaping symlinks fall back to identity). The sidecar
  inherits the source asset's name, content type, cache policy, and
  modtime — conditional requests key to the content version, and
  immutable hashed bundles stay immutable in both representations.
  `Vary: Accept-Encoding` rides every asset response; the injected SPA
  shell is excluded by construction (its bytes vary per session, so no
  precompressed representation can exist). Zero config, zero CPU —
  operators who pre-compress their web root get compressed transfer for
  free; everyone else sees byte-identical behavior plus one header.
- **2026-09-23** — Phase 5h: embedded admin console (ADR-0080), resolving
  the ADR-0054 follow-up. Operators who never set `web.static_root` had no
  browser surface at all — no zero-config way to answer "is the instance
  up, who has an account, what is the job runner doing". The console is
  the ADR-0054 middle path, not the rejected full-frontend embed: three
  hand-written files (`index.html`/`console.js`/`console.css`, vanilla
  HTML/JS/CSS, ~250 lines, no framework, no build step) embedded via
  `embed.FS` and served at `/console` — a namespace Nextcloud never
  claims, so it can never shadow the served frontend (the router's
  exact/longest-prefix rules keep it ahead of the static catch-all, pinned
  by an app-level test). Authorization is NC's `admin`-group convention:
  `EnsureBootstrapAdmin` now creates the group and the membership on the
  empty-database path only (existing installs untouched; NC-imported
  instances inherit NC's own admin group), and `console.RequireAdmin`
  gates every route on it after the usual credential stack. v1 is
  read-only — GET/HEAD only, so zero CSRF surface; the three JSON
  endpoints (status reusing the shared `status.Provider`, paged users,
  recent jobs via a new `jobs.SQLStore.ListRecent`) clamp pagination and
  render RFC3339 UTC timestamps; per-user groups are deliberately omitted
  (N+1). Mutations are future work behind ADR-0064 requesttokens.
- **2026-09-23** — Phase 5g: addressbook sharing (ADR-0079), resolving the
  ADR-0071 skip: `oc_dav_shares` rows with `type='addressbook'` had no
  ncgo share table to land on, and CardDAV had no sharing at all. The
  increment mirrors the Phase 3e3 calendar-sharing model line for line —
  one proven model for both DAV families, exactly as NC runs one
  `dav_shares` backend per resource type. Migration 0019 adds
  `addressbook_shares` (mirrors `calendar_shares`) in all three dialects;
  the contacts store, DAV (cs:share POST, `{uri}_shared_by_{owner}`
  naming, shared-book resolution in Stat/List/Read/Write/Remove and both
  REPORTs), and PROPFIND (`share-access` / `owner-principal` for shared
  addressbooks) follow the calendar shape, including the rules: own books
  only, immediate effect (NC `dav_shares` carries no invite state),
  read-write gating for sharee writes into the owner's book, and no
  sharee collection delete (403). The importer's addressbook-shares skip
  branch becomes a real mapping with the calendar classification rules,
  the resolved-set gate extended to addressbooks so a share never
  attaches to a foreign book that merely owns the same uri.
- **2026-09-23** — Phase 5f: DAV MKCOL parent-missing 409 (ADR-0078),
  resolving the ADR-0067 follow-up. RFC 4918 §9.3.1 wants 409 Conflict
  when the parent collection does not exist — the signal the desktop
  client's mkdir discovery walks up on. The webdav layer was always
  ready (`writeFSError` maps `ErrParentMissing` to 409, mock-FS tested);
  the gap was one layer down in `files.DAV.mkdirOwned`, where
  `mapStorage` sent `storage.ErrNotFound` to 404. In the mkdir context a
  missing storage entry can only be the parent (an existing target
  yields `storage.ErrExists`, already tolerated), so the call-site
  translation to `webdav.ErrParentMissing` is exact; `mapStorage` keeps
  its 404 for every operation that legitimately needs it. The
  incoming-share path inherits via `mkdirOwned`; S3 (no-op Mkdir, 409
  via filecache `Meta.Insert`) and localfs now agree. PUT/MOVE/COPY
  parent-missing stay as-is until a captured client flow shows otherwise.
- **2026-09-23** — Phase 5e: extensionless `/core/preview` mounts
  (ADR-0077), resolving the ADR-0069 follow-up. The 4w SPA bootstrap
  advertises `modRewriteWorking: true`, so the NC web frontend strips the
  `/index.php` prefix from its generated preview URLs — and every such
  request 404'd against ncgo, which only mounted the `/index.php/...`
  pair. `GET /core/preview` and `/core/preview.png` are now mounted
  alongside, same `webdav.Auth` middleware, same path-agnostic
  `preview.Generator` handler (two route lines, no handler change). A
  generic index.php-stripping middleware was rejected: explicit mounts
  keep the route table — and the router-named tracing spans — honest.
  Other pretty URLs join only when a captured client request shows a
  real 404 (the wire-compat evidence rule).
- **2026-09-23** — Phase 5d: dedicated metrics listener (ADR-0076),
  resolving the last deferred item of ADR-0055. The
  `observability.metrics_listen` key — removed in Phase 4k as
  defined-but-dead — returns: when set, `/metrics` is served on a dedicated
  HTTP listener at that address instead of the main listener. Move, not
  copy: the separate listener exists precisely for network-level
  restriction (bind localhost or an inner interface while the main listener
  is public), so the main-router mount is skipped and only the token-guarded
  dedicated mux answers scrapes. Validation pins the 4k principle —
  `metrics_listen` requires `metrics_enabled` (a listener without metrics
  would be another silent no-op, now a startup-fatal error that says so)
  and must parse as host:port (empty host = all interfaces, matching
  `server.listen`). The dedicated server is a stdlib mux serving only
  `GET /metrics` (other paths 404, other methods 405 for free from the Go
  1.22+ pattern; no /healthz, no /status) on a second `httpx.Server` with
  default timeouts, built by a testable `App.metricsServer()` helper.
  Lifecycle: `App.Run` derives a cancellable context, runs the metrics
  server in a goroutine on a buffered error channel and the main server in
  the foreground; a metrics failure (e.g. bind error) cancels the context
  so the main server shuts down gracefully and the metrics error is
  returned, while normal shutdown collects the goroutine's result and
  `errors.Join`s it. `TestLoadUnknownKeysIgnored` drops the key (live
  again, mirroring the Phase 4z `otel_endpoint` precedent), full.yaml
  gains the `metrics_enabled` + `metrics_listen` pair, and ADR-0055's
  deferred list is now empty. Zero new dependencies.
- **2026-09-23** — Phase 5c: database query spans (ADR-0075), resolving the
  DB-spans follow-up ADR-0072 pinned. `database.WithTracing(inner, tp)`
  decorates the DB interface — the single choke point every store hangs
  off — chosen over driver-level otelsql (no contrib dependency beyond the
  ADR-0072 approval, and the decorator sees the pre-Rebind `?`-placeholder
  SQL, the static-per-call-site form) and over per-store wrapping (one wrap
  at the source covers all stores by construction). `App.New` now builds
  the TracerProvider immediately after `database.Open` and migrations (the
  `instanceID` assignment moved ahead of it for the resource's
  `service.instance.id`) and wraps the pool before any store is built, so
  users, sessions, shares, DAV meta, jobs, appconfig, the plugin registry,
  and the plugin host's DB access are all traced by the single wrap; a nil
  provider returns the inner DB untouched, keeping disabled tracing exactly
  zero overhead. Spans are SpanKindClient named by the uppercase first SQL
  keyword (`SELECT`/`INSERT`/`BEGIN`/...; empty statement → `SQL`;
  statement text never in the name) with `db.system`, `db.operation`, and
  `db.statement` attributes (truncated at 4096 bytes, rune-safe); Exec
  success adds `db.rows_affected`, Query end adds `db.rows_returned`.
  Errors record an exception plus Error status, except QueryRow's
  `ErrNoRows` — a normal not-found, mirroring the 4xx-is-not-Error stance.
  Lifecycle ends every span exactly once (sync.Once): Query on Close or
  iteration exhaustion, QueryRow at the first Scan (a never-Scanned Row is
  a missing span, never wrong data), Exec/Begin/Commit/Rollback/Ping at
  return, with in-transaction spans parented to BEGIN; Close stays
  untraced. No new config — `otel_endpoint`/`otel_sample_ratio` govern (the
  per-query span volume is why ratio < 1.0 exists for busy instances); the
  CLI is deliberately untraced (short-lived process, no provider
  lifecycle). ADR-0072's DB-spans bullet struck through as resolved.
- **2026-09-23** — Phase 5b: server-encryption key rotation (ADR-0074),
  resolving the key-rotation follow-ups ADR-0052 and ADR-0070 pinned. The
  sealed-file header gains a format version: v2 is `NCGOENC2` + a 1-byte
  key ID + the 32-byte salt (41 bytes; v1 files implicitly carry key ID
  0), because GCM failure cannot distinguish wrong-key from corruption
  and try-all-keys reads would be O(n) per chunk — the ID byte makes the
  lookup O(1) and the config error explicit (new sentinel
  `ErrUnknownKeyID`, never conflated with `ErrIntegrity`). `encrypt.FS`
  becomes an append-only keyring (`NewWithPrevious(current, previous,
  inner)`; positional IDs, previous keys read-only, the current key seals
  at the highest ID) fed by the new `encryption.previous_key_paths`
  config list — append-only forever, since reordering or removing an
  entry re-keys/orphans the files sealed under those IDs by design.
  Validation at both layers: 32-byte keys, ring ≤ 256, no byte-duplicate
  keys, no blank/duplicate/master-repeated paths. Single-key rings write
  v1 headers bit-identical to pre-rotation builds (rollback-safe; zero
  format change for non-rotating deployments); multi-key rings write v2,
  and mixed v1/v2 trees are the normal rotation state. The sweep gains a
  third direction `SweepRotate` — plaintext is skipped (encrypt-all
  composes, sealing straight onto the current key), files under retired
  IDs are read through the ring and re-sealed under current, aborts hit
  on `ErrIntegrity`/`ErrUnknownKeyID` at the first file, single-key rings
  are rejected, dry-run counts via `enc.Stat`. CLI: `ncgo-cli encryption
  rotate-keys [--user] [--dry-run]` shares the sweep wiring, builds the
  keyring once for all sweep subcommands, and errors with the full
  rotation procedure when no previous key is configured; `encryption
  status` reports previous-key count, per-file loadability (fail-closed),
  and the current key ID; the parent help documents the five-step
  procedure. Remaining follow-ups dispositioned: filename encryption and
  per-user keys deferred (each needs its own design phase), S3 SSE-C
  rejected as redundant with the ADR-0052 decorator.
- **2026-09-23** — Phase 5a: import-nextcloud tokens (ADR-0073), resolving
  the `tokens` follow-up ADR-0071 §Decision 3 pinned. New
  `import-nextcloud tokens` subcommand imports `oc_authtoken` rows with
  `type = 1` (permanent app passwords) into `app_passwords`: the
  sha512(token+secret) hash is copied **verbatim** (ncgo's hashing is
  byte-identical to Nextcloud's by design, so imported tokens verify
  against the original app password if and only if `instance.secret`
  equals the source config.php `secret` — copied before first use and
  kept forever; rotating it invalidates every imported token at once),
  `id` synthesizes to `nc-<id>`, `last_activity` seconds become
  `created_at` ms (NULL → 0), `login_name` falls back to the mapped uid,
  and the password/keypair columns are dropped (ncgo never decrypts
  stored passwords). Browser sessions (type 0) stay not importable;
  wipe tokens (type 2) are filtered with them in SQL. The source uid
  maps exactly like the users importer (uid → uid_lower fallback);
  tokens for unmappable or missing-target users skip with per-row
  warnings — a mandatory `GetByUID` pre-check covers `auth.Insert`'s
  silent 0-rows-on-unknown-uid behavior. Idempotent on the token hash
  (`GetByHash`; also skips rows carried over by hand via the ADR-0071
  SQL recipe), resumable, dry-run counts without writing, and a missing
  `authtoken` table is a hard error. Tests include the cross-instance
  money test: a fixture row hashed with the source secret authenticates
  through `AppPasswordVerifier` configured with the same secret — and
  fails with a different one.
- **2026-09-23** — Phase 4z: OpenTelemetry spans (ADR-0072), resolving
  the ADR-0055 OTel follow-up and restoring the `observability.otel_endpoint`
  key Phase 4k had removed as dead. The project-wide zero-dependency posture
  is opened **by explicit user approval (2026-09-23) for exactly three
  modules**: `go.opentelemetry.io/otel`, `.../otel/sdk`, and
  `.../otlp/otlptrace/otlptracehttp` (pinned at v1.44.0 — the version the
  module graph already selected, so no other dependency moves; no gRPC
  exporter, no contrib). `observability.otel_endpoint` (empty = completely
  uninstalled, zero overhead; bare `host:port` = plaintext HTTP collector,
  full URL keeps scheme+path) plus `observability.otel_sample_ratio`
  (parent-based head sampling, default 1.0, validated to [0,1]) drive a
  BatchSpanProcessor provider owned by `App` (Shutdown with a 5s flush
  budget joined into the Close error chain). A ~80-line self-written
  middleware (in lieu of contrib otelhttp) sits directly behind
  `httpx.Recover` in the base chain: W3C traceparent extraction, low-cardinality
  span names (`GET /status.php` from the registered route the router puts on
  the request context — never the raw path, which lands only on
  `http.target`), and 5xx → Error status. Plugin host calls get an
  independent `plugin.host.<function>` span layer at the 4i wrapHostMetrics
  hook (`plugin.id`/`ncgo.function`/`ncgo.result_code`, only ErrCodeInternal
  marks Error), composed with — and switched separately from — the metrics
  wrapper. DB spans remain a follow-up.
- **2026-09-23** — Phase 4y: import-nextcloud completion (ADR-0071),
  resolving the ADR-0048 email/quota and ADR-0051 calendar-shares
  follow-ups. `import-nextcloud users` now maps email
  (`oc_preferences` `settings/primary_email` → `settings/email` →
  `oc_accounts.data` JSON fallback — `oc_users` never had an email or
  quota column in any Nextcloud release) and quota (`files/quota`
  preference: `none`/`default`/empty → NULL, 1024-based human sizes →
  bytes, invalid → NULL + warning). `import-nextcloud dav` replaces the
  counted-warning stub with a real calendar-share mapping: research
  pinned `oc_dav_shares` as the only share table Nextcloud ever had
  (`access` 2 = read-write, 3 = read; `type` also covers addressbooks;
  `resourceid` → `oc_calendars.id`; **no invite state** — every row is an
  effective share, so the ADR-0051 never-show-unaccepted principle holds
  structurally). Only the unambiguous subset imports: user principals,
  sharee in target, and a calendar the run actually imported (a new
  resolved-set gate prevents shares attaching to a foreign calendar that
  owns a colliding uri); group/circle principals, orphans, unmappable
  access, and self-shares skip with per-row warnings; addressbook shares
  are counted (ncgo has no addressbook sharing). Idempotent re-runs skip;
  differing access updates per `UpsertCalendarShare`. Authtoken verdict:
  sessions not importable (ncgo session family is its own); app passwords
  are hash-compatible by design (`sha512(token+instance.secret)` on both
  sides) — carry the source `secret` over and copy rows manually per the
  ADR recipe, or (default) reissue after migration; a `tokens` subcommand
  is the one remaining follow-up.
- **2026-09-23** — Phase 4x: `encryption encrypt-all`/`decrypt-all` CLI
  sweep (ADR-0070), resolving the ADR-0052 follow-up. Transparent
  seal-on-write left legacy plaintext on the backend until organic
  rewrites; the new sweep re-encodes the whole storage tree (or one user's
  `<uid>/` subtree via `--user`) in place: sniff the magic per file, skip
  files already in the target encoding (idempotent, re-runnable after
  interruption), read each remaining file in full and write it back through
  the other layer — `Create` replaces atomically (localfs rename, s3
  put-on-close), so concurrent readers through the server's auto-detecting
  wrapper always see correct content and no downtime is required (low
  traffic recommended against concurrent-write loss). filecache, versions,
  and etags are deliberately untouched: the plaintext content — hence every
  metadata value they track — is byte-identical, only the encoding at rest
  changes. Per-file failures are counted, reported, and skipped (exit
  non-zero when any); a wrong master key in `decrypt-all` aborts at the
  first sealed file with a key-mismatch error instead of failing every
  file. The sweep logic lives in `internal/storage/encrypt/sweep.go`
  (`Sweep(ctx, raw, enc, opts)` with Direction/Prefix/DryRun/Progress/
  OnError); the CLI is thin wiring over a new `openRawBackend` helper split
  out of `deps.go`'s `openStorage` (existing callers unchanged), requiring
  `encryption.enabled` and a loadable key. `--dry-run` counts what would
  change with zero writes. Key rotation, per-user keys, filename
  encryption, and SSE-C remain open follow-ups.
- **2026-09-23** — Phase 4w: SPA bootstrap initial-state injection
  (ADR-0069), closing the ADR-0054 "server-rendered bootstrap state"
  follow-up at the core-subset level. ADR-0064's single shell-injection
  pipeline is extended (not duplicated): `ShellBootstrap` now resolves a
  `(requesttoken, BootstrapState)` pair per request, and `serveShell`
  injects one extra `<script>` block with the upstream-named globals the
  compiled frontend reads at boot — `window._oc_webroot` ("" — ncgo serves
  at root; kills `webroot.js`'s wrong pathname deduction on SPA routes),
  `window._oc_config` (exactly `modRewriteWorking: true` — ncgo mounts
  OCS/DAV extensionless — plus `session_keepalive`, `session_lifetime`
  from the same 24 h default the login handlers apply, and
  `version`/`versionstring` from `internal/version`, the status.php
  source), and `window.oc_appconfig.core` with upstream's exact 16-key
  share-default set held at ncgo-faithful values (no expiry enforcement,
  optional link passwords, re/group/remote sharing allowed). Session
  personalization is head attributes only — `data-user` /
  `data-user-displayname` behind the auth middleware's exact validity
  criteria, user menu shows the current user — and anonymous shells strip
  any operator-baked copies of those attributes; the script payload is
  byte-identical across states (test-pinned), so nothing user-specific
  enters a JS string context, and `encoding/json`'s `<`/`>`/`&` escaping
  makes `</script>` breakout impossible by construction. Key-set research
  is pinned to upstream sources (layout.user.php, JSConfigHelper
  stable26/28/master, core/src consumers, nextcloud-initial-state) with
  per-key rationale and the rejected-key list in the ADR; initial-state
  hidden inputs are evaluated and deferred with an explicit trigger
  condition. Accepted casualty, recorded as follow-up: `modRewriteWorking:
  true` breaks the files app's `/core/preview` URL shape until an
  extensionless preview mount lands. `internal/version` gains `String()`.
- **2026-09-23** — Phase 4v: plugin cross-instance handle aggregate caps
  (ADR-0068), closing the ADR-0060 aggregate-cap follow-up. The §8
  open-handle budgets (64 stream / 16 DB rows / 16 HTTP response) are
  re-scoped from per instance to per plugin, shared across all of the
  plugin's live instances: pooled (`pool_size`) and per_request (request
  concurrency) models previously multiplied the footprint, and open rows
  handles pin `database/sql` pool connections outside the ADR-0060
  statement slots. The host holds a `(plugin id, kind) → count` aggregate
  acquired in `handleTable.add` (table check first; aggregate refusal
  consumes nothing, so no leak on either refusal path) and returned in
  `remove`/`closeAll` — a trap-destroyed instance returns its slots
  exactly, pinned by wasm-level fill/trap/refill probes and a `-race`
  churn test. Handle ids stay instance-scoped; exhaustion still answers
  -12, so guests are unaware. Manifest validation gains the missing
  `pool_size` upper bound (1..32 accepted; each pooled instance is a live
  wasm module with its own linear memory). No new config keys or metric
  families — refusals land in the existing §12 `result` label.
- **2026-09-23** — Phase 4u: plugin `storage_mkdir` host function +
  per-plugin storage byte metrics (ADR-0067), closing two ADR-0042/0061
  storage follow-ups. `storage_mkdir(path_ptr, path_len) -> i32` creates a
  single directory (no implicit parents) under the scope's write grant: user
  scope through the DAV (filecache-consistent, incoming-mount aware), system
  scope through the plugin's system tree; existing target → -5, missing
  parent → -4 (backed by `localfs.Mkdir` now mapping that case to the
  `storage.ErrNotFound` sentinel like `Stat`/`Open` do), and directories
  carry no bytes so no quota applies. The §12 metrics surface gains
  `ncgo_plugin_storage_bytes_total{plugin, op, scope}` (op ∈ read|write,
  scope ∈ user|system) via a new `Registry.AddCounter` (non-positive deltas
  no-op), counted where bytes actually move: per stream read (the open
  handle now carries its scope) and per successful stream-close commit —
  quota-refused commits count nothing; nil registry stays zero overhead and
  the plugin id follows the 4n/4o nil-guard. Chunked/resumable plugin writes
  are conditionally deferred (spool covers 1 GiB under quota; plugin state
  is small; the DAV chunked machinery serves human clients; a plugin-side
  chunked ABI is not worth its surface in v1 — reopens on a concrete plugin
  need). ABI stays `ncgo-abi/1` per §9; pluginsdk gains `StorageMkdir`.
- **2026-09-23** — Phase 4t: plugin outbound request body streaming
  (ADR-0066), closing the ADR-0043 ">1 MiB uploads" follow-up. Three new
  host functions stage a body in a temp-file spool —
  `http_request_body_create` (gated on `http.outbound` like `http_request`),
  `http_request_body_write` (chunk append; -11 above the 1 MiB `buf_len` or
  past the spool cap), `http_request_body_close` (seal; later writes and
  double closes → -2) — capped by the existing `HostConfig.MaxSpoolBytes`
  (default 1 GiB) and sharing the §8 64-stream handle budget. The
  `http_request` map gains `body_handle`, mutually exclusive with
  `body_bytes` (both → -2); the handle must be sealed (unsealed → -2, stays
  usable), and consumption is destructive: the request goes out with a
  known `ContentLength` plus a `GetBody` reopening the spool for 307/308
  replay, and the spool is deleted when the call ends (never-consumed
  spools die with the instance handle-table cleanup). Spool over live
  `io.Pipe` streaming because the guest only runs inside host calls while
  `client.Do` reads the body asynchronously, the per-call timeout model has
  no owner for a cross-call upload, and mid-upload upstream failures cannot
  travel back to the guest. Allowlist, egress IP guard, rate limit,
  redirect re-validation, and the response byte cap apply unchanged;
  allowlist/rate denials precede consumption so a refused guest keeps its
  spool. ABI stays `ncgo-abi/1` per §9; pluginsdk gains
  `HTTPBodyCreate`/`HTTPBodyWrite`/`HTTPBodyClose` and an `omitempty`
  `BodyHandle` field on `HTTPOutboundRequest`.
- **2026-09-23** — Phase 4s: plugin route request body streaming
  (ADR-0065), closing the ADR-0041 `body_handle` follow-up as a manifest
  opt-in. Plugins with `runtime.request_body_stream = true` receive the
  specced §7 request map (`body_handle`, no inline `body_bytes`): dispatch
  wraps `req.Body` as a handle on the instance's `handleStream` table
  (sharing the §8 64-stream budget) before `ncgo_on_request` runs, and the
  guest pulls the body via the two new host functions
  `request_body_read`/`request_body_close` (mirroring
  `http_response_body_read`/`http_response_close`: -11 above the 1 MiB
  `buf_max`, -4 missing, -2 foreign type). The 1 MiB inline cap — and its
  413 — no longer applies to opt-in plugins; non-opt-in plugins are
  byte-identical to 4c3. Leftover handles are dropped by the instance
  handle-table cleanup (the wrapper's Close is a no-op; `req.Body` stays
  owned by net/http). ABI stays `ncgo-abi/1` per §9 (new functions within a
  major); pluginsdk gains `RequestBodyRead`/`RequestBodyClose` and a
  `BodyHandle` field on `HTTPRequest`. Outbound streaming (>1 MiB
  `http_request` uploads) remained a follow-up, closed by Phase 4t above.
- **2026-09-22** — Phase 4r: requesttoken + CSRF validation + SPA browser
  login (ADR-0064), completing two ADR-0054 follow-ups. Per-session CSRF
  tokens are derived statelessly as
  `base64url(HMAC-SHA256(instance.secret, "ncgo-requesttoken:"+sessionID))`
  (no schema change, constant-time compare); the anonymous login page uses a
  double-submit `ncgo_login_nonce` cookie in a separate HMAC domain. The
  static SPA shell (`/` and SPA fallback) is now injected with the token at
  both upstream read paths — `<head data-requesttoken>` and
  `window.oc_requesttoken` (conventions verified against nextcloud/server
  templates and @nextcloud/auth) — while other assets stay byte-identical;
  injected shells drop conditional-request negotiation (always 200,
  no-cache). New endpoints: `POST /index.php/login` (login token check →
  403, credential failure → 401 via the shared CacheThrottler, success →
  session + nonce rotation + 303) and `GET|POST /index.php/logout`
  (session-token check → delete session → 303); `GET /index.php/login`
  serves the shell explicitly because the router 405s exact-path method
  mismatches. CSRF validation lives in the auth middleware's session branch:
  session-authenticated unsafe methods (safe set GET/HEAD/OPTIONS/PROPFIND/
  REPORT) need a matching `requesttoken` header or form field (body restored
  for downstream readers) else 403; basic/app-password/bearer/public-link
  auth is exempt. `httpx.CSRF` defers session-cookie requests to that core
  (new SessionCookie config) and keeps its anonymous 412 blanket; login and
  logout paths are bypassed as self-validating. Coverage spans DAV, OCS, and
  plugin routes since all share `auth.Middleware`. Still follow-ups: full
  `oc_appconfig` initial-state injection, precompressed assets, embedded
  admin console.
- **2026-09-22** — Phase 4q2: reconciler cache purge on uninstall detection
  (ADR-0063). Hot reload (4q) broke ADR-0058's "memory L1 residue dies with
  the restart" argument — a same-process uninstall → reinstall would read
  the previous generation's `plugin:<id>:` keys, and the CLI-side purge only
  covers Redis deployments. The reconciler now distinguishes the stop cause
  by re-reading the registry: row gone (uninstall) → purge the plugin's
  cache keys through the host cache; row present (disable/upgrade) → keep,
  matching the jobs/appconfig/storage semantics. Registry read failures skip
  conservatively; purge failures warn without interrupting the stop.
- **2026-09-22** — Phase 4q: plugin hot reload (ADR-0062), closing the
  ADR-0041 boot-time-mount follow-up. A new `plugins.Reconciler` polls the
  registry every `plugin.refresh_interval` (new config key, default 10s;
  0 = restart-only, negatives rejected) and reconciles the running set:
  newly enabled plugins are started and mounted (the former
  `plugins.StartEnabled` + `MountRoutes` boot pair is now the reconciler's
  first `Sync` inside `mountRoutes`), disabled/uninstalled plugins are
  unmounted and stopped, and a registry version change (upgrade, with the
  CLI's 4j clear-then-hook having already rewritten the route rows) is
  stop-then-start. Stops remove the tracked route keys recorded at mount
  time (uninstall deletes the route rows first, so the DB cannot answer),
  `Unregister` the `plugin.<id>` job adapter (new `jobs.Runner.Unregister`;
  queued rows retry as unknown-name and drop after three strikes), then
  `Plugin.Close`. `httpx.Router` is now safe for concurrent register/Remove
  vs ServeHTTP (one read lock resolves the handler; invocation happens
  unlocked) and gained exact-route `Remove`. Per-plugin failures stay
  isolated and are retried next pass; a registry read failure keeps the
  running set. In-flight requests already dispatched to a removed route run
  to completion. The 4k "restart required" texts (README, both example
  READMEs, CLI enable/disable help) now read "takes effect within
  `plugin.refresh_interval`". Follow-up: with hot reload, ADR-0058's
  memory-cache uninstall residue no longer dies with a mandatory restart —
  a reconciler-side `plugin:<id>:` cache purge on detected uninstall is
  open.
- **2026-09-22** — Phase 4p: plugin storage quotas (ADR-0061), closing the
  ADR-0042 "quota checks" follow-up (chunked/resumable writes, per-plugin
  storage byte metrics, a mkdir host function, and core-DAV quota
  enforcement remain follow-ups). Both `storage_*` write scopes check at
  `storage_create` (declared size; skipped when the guest declares <= 0) and
  again at `storage_stream_close` against the actual bytes. User scope
  refuses when `usage - oldSize + newBytes > users.quota_bytes` (NULL =
  unlimited) using the filecache `Store.Usage` sum through a new
  `files.DAV.Usage` pass-through — no new config key, quota stays user data
  managed by the existing CLI user commands. System scope captures the
  plugin tree size at create via a bounded recursive `List` walk
  (`systemTreeUsage`, sharing deleteTree's depth/entry caps after the
  constants were generalized) and wraps the create stream in a counting
  `systemQuotaWriter` so close enforces the actual byte count, deleting the
  just-committed partial content on refusal. Overwrites pay only the delta
  (`oldSize` from Stat, 0 when absent). Refusals return -8 plus a warn log
  naming the plugin, scope, and numbers — never routed through
  `mapStorageErr`, so a denial is never flattened to -1 — and classify into
  the existing `result` label. New `HostConfig.PluginSystemQuotaBytes` (<= 0
  defaults to 1 GiB, matching the per-file spool cap) wired to
  `plugin.system_storage_quota_mb` (default 1024, negatives rejected) and
  passed through by `internal/app` and the `ncgo-cli` install host (install
  hooks write system storage). wasmgen gains `StorageCreateSizeProbeModule`
  and `StorageWriteCloseProbeModule` (registered in `TestModulesCompile`) —
  the existing probes pass declared size -1 and drop the close result, so
  neither checkpoint was observable. Tests: tree-walk unit tests (nested,
  empty, missing root/target, dir target), HostConfig defaulting, user
  create/commit refusal + warn, nil-quota unlimited, overwrite delta
  (success and refusal leaving the old file), system create/commit refusal
  with partial-file and backend-temp cleanup; config defaults snapshot,
  full parse, negative validation. Spec Change Log and Phase 0 blueprint
  config YAML updated.
- **2026-09-22** — Phase 4o: per-plugin DB concurrency quota for the plugin
  host (ADR-0060), closing the ADR-0039 "per-plugin connection
  pools/quotas" follow-up in its quota form (per-plugin pools and
  read/write splitting remain follow-ups; query duration in §12 metrics was
  already closed by 4i's wrapHostMetrics, no code change needed). The five
  connection-consuming entries — `db_query`, `db_exec`, `db_tx_query`,
  `db_tx_exec`, `db_tx_begin` — acquire a slot from a per-plugin-id counter
  (`Host` `dbConcMu` + `map[string]int`, zero-count keys deleted, bounded
  by installed plugin count, reset on restart) after the capability/SQL
  checks and `defer` its release around the `Query`/`Exec`/`Begin` call, so
  the quota governs in-flight statements only; open rows/tx handles stay
  under the per-instance handle budget and a cross-instance aggregate
  handle cap is a documented follow-up (bounded in practice: 16
  handles/instance, operator-reviewed manifest pool sizes). Exhaustion
  returns -8 (`ErrQuotaExceeded`) plus a warn log naming the plugin — the
  4n rate-limit posture — and classifies into the existing bounded `result`
  label (no new metric families). `HostConfig.DBMaxConcurrentPerPlugin` <= 0
  defaults to 4 (bounds concurrency, not throughput; slot lifetime is
  capped by the per-call wall-clock timeout). New config key
  `plugin.db_max_concurrent_per_plugin` (default 4, negative rejected)
  passed through by both `internal/app` and the `ncgo-cli` install host —
  install/upgrade hooks run DDL/DML through `db_*` and must not bypass the
  quota. Tests: helper boundary/isolation/zero-delete/defaulting unit
  tests; wasm integration with quota 1 (held slot forces -8 + warn log;
  uncontended sequential statements succeed). Spec Change Log and Phase 0
  blueprint config YAML updated.
- **2026-09-22** — Phase 4n: per-plugin rate limit + response size cap for
  plugin outbound HTTP (ADR-0059), closing two of the three ADR-0043
  follow-ups (only >1 MiB streaming request bodies remain, an ABI extension
  tracked separately). `httpRequest` draws one token per call — redirect
  chain included — from a per-plugin-id stdlib token bucket (no new
  dependency): `HostConfig.HTTPRatePerMinute` default 120, burst 30, with
  exhaustion returning -8 (`ErrQuotaExceeded`) plus a warn log naming the
  plugin; buckets are lazily created, bounded by the installed plugin count,
  and reset on restart. Response bodies are capped at
  `HostConfig.MaxHTTPResponseBytes` (default 32 MiB) by a counting
  `limitedBody` wrapper: the read that would cross the cap fails loudly with
  -11 (`ErrTooLarge`) instead of silently truncating, and later reads keep
  failing. Both limits are orthogonal to the ADR-0057 egress guard and apply
  to the guarded and unguarded clients alike. New config keys
  `plugin.http_rate_per_minute` (120) and `plugin.max_http_response_mb` (32)
  with non-negative validation are passed through by both `internal/app` and
  the `ncgo-cli` install host (install hooks can call `http_request`); the
  burst knob stays a HostConfig-only field (same precedent as
  `MaxSpoolBytes`). No new metric families — the ADR-0055 wrapper classifies
  the new refusal codes into the existing bounded `result` label. wasmgen
  gains the `HTTPBodyCapModule` probe (registered in `TestModulesCompile`)
  because the existing body loop cannot distinguish a -11 from EOF. Spec §6,
  the wasm-plugin-abi Change Log, and the Phase 0 blueprint config YAML
  updated.
- **2026-09-22** — Phase 4m: `cache.DeleteByPrefix` + plugin uninstall cache
  cleanup (ADR-0058), closing the ADR-0056 Deferred item 1. `cache.Cache`
  gains `DeleteByPrefix(ctx, prefix) (int64, error)` — empty prefix rejected
  (`ErrEmptyPrefix`, no `MATCH *` footgun) — with all three implementations:
  Memory keeps a `keys` index beside the `Increment` mutex (ristretto has no
  key iteration and its callbacks report only the hashed key, so values now
  carry their key string; `OnEvict`/`OnReject` untrack, `Set` tracks before
  entering the set buffer), Redis runs a SCAN cursor loop with the prefix
  glob-escaped (`\ * ? [ ]`) plus batched non-blocking UNLINK, and Tiered
  sweeps both layers reporting the L2 count (layers mirror one keyspace, so
  summing would double-count). `Installer.Uninstall` now purges
  `plugin:<id>:` keys right after the appconfig step via a prefix helper
  shared with the write path, logging the count; the CLI injects a
  `cache.Redis` only when `cache.redis_addr` is configured (memory-only
  residue dies with the server restart uninstalls already require). The
  actual hazard was Redis L2 surviving restarts: a reinstalled plugin would
  have read the previous generation's keys. Spec and wasm-plugin-abi Change
  Logs updated.
- **2026-09-22** — Phase 4l: plugin HTTP egress private-IP guard (SSRF,
  ADR-0057), the ADR-0043 follow-up that checks spec §13's last unchecked
  egress item. Plugin `http_*` outbound dials now run a
  `net.Dialer.Control` hook over the resolved IP and refuse loopback,
  private (RFC1918 + ULA), link-local, and unspecified targets — closing the
  literal-internal-IP (cloud metadata 169.254.169.254) and DNS-rebinding
  bypasses of the hostname allowlist, with no TOCTOU window. Authorization
  splits at client selection because Control has no request context:
  plugins granted the new manifest boolean
  `http.outbound_allow_private = true` use the configured
  `HostConfig.HTTPClient` as-is, everyone else a guarded clone on a
  separate transport (connection pools never mix authorization levels).
  Escape hatches: custom RoundTripper or operator-set Dial/DialContext
  disables the guard (debug log, operator owns egress policy); proxied
  deployments see the proxy's address. CGNAT 100.64/10 and multicast are
  deliberately not blocked. Refusals map to -3 with a warn-level security
  log naming the plugin. The webhook-forwarder example declares the grant
  (its default `localhost:8080` receiver is a private target). Spec §5/§6/§13
  and the wasm-plugin-abi Change Log updated.
- **2026-09-22** — Phase 4k: dead config key removal + plugin restart doc
  sync, from the same strict review line as Phase 4j. Eight keys were
  defined in `internal/config`, given defaults, and documented but never
  read by any non-test code — operators could set them and nothing
  happened (silent no-ops). Removed (re-introduce with the feature that
  consumes them): `server.trusted_proxies`, `server.trusted_domains`,
  `server.base_url`, `auth.session_ttl`, `auth.app_password_ttl`,
  `auth.password_hash`, `observability.metrics_listen`, and
  `observability.otel_endpoint`. The last two were the ADR-0055
  acknowledgments (separate metrics listener unwired, OTel deferred); the
  ADR now records their removal and that both return when the separate
  listener / OTel SDK land. **Removal is NOT a breaking change for
  existing YAML files**: `config.Load` unmarshals leniently (koanf's
  default mapstructure decoder does not set `ErrorUnused`, and `Validate`
  has no unknown-key rule), so files still setting these keys load without
  error and the values are silently ignored — the same practical effect
  they always had; `TestLoadUnknownKeysIgnored` pins that contract. Also
  from the review: `ncgo-cli plugin enable|disable` help now states a
  server restart is required (the running server reads the enabled plugin
  set once at boot via `plugins.StartEnabled`), with matching notes in
  README.md and the file-tagger / webhook-forwarder example READMEs.
- **2026-09-22** — Phase 4j: plugin lifecycle gap fixes (ADR-0056), from a
  strict review of the Phase 4 stack. (G1) `ncgo-cli plugin install` no
  longer uses an empty host: the CLI now builds the full install-time
  plugin host (DB, route/OCS/WebDAV-prop registry, appconfig, jobs runner,
  event bus, files DAV via a helper shared with `import-nextcloud files`,
  system storage under `appdata_<instance.id>/plugins`), so the shipped
  file-tagger example — and any hook using db/config/jobs/storage/routes —
  actually installs; cache stays nil deliberately (management surface,
  runtime state) and `plugin check` keeps its empty-host smoke-test role.
  pluginsdk gained the missing RouteRegister/OCSRegister bindings, and
  install/upgrade register the `plugin.<id>` job adapter before hooks run
  so hook-time `job_enqueue` works. (G2) `ncgo_on_upgrade` is no longer
  dead: upgrades follow clear-then-hook ordering (delete the old version's
  route/prop rows, then invoke on_upgrade with the from-version string as a
  lifecycle hook, before storing the new archive), with an on_install
  fallback for plugins without an upgrade hook. (G3) uninstall now removes
  the plugin's jobs rows, `<id>.*` appconfig rows, and system-storage tree
  (bounded recursive delete), and the jobs runner drops unknown-name rows
  after 3 failed attempts instead of rescheduling them forever — plus a
  one-line `app.Close` fix that no longer discards earlier close errors
  when the plugin host closes. Newly confirmed spec deviations recorded in
  the spec Change Log: `fuel_per_call` parsed-but-unenforced, per-plugin
  `memory_limit_mb` not applied, trap-during-request is 502 (spec text
  corrected), §13 private-IP egress check still pending; cache-key cleanup
  on uninstall remains a documented follow-up (cache.Cache has no prefix
  delete).
- **2026-09-22** — Phase 4i: Prometheus metrics for the plugin system
  (ADR-0055), completing the Phase 4 feature set. A minimal stdlib-only
  Prometheus text-exposition registry (`internal/observability.Registry`,
  zero new dependencies — the project deliberately does not take on
  `client_golang`) backs the three §12 families:
  `ncgo_plugin_host_calls_total{plugin,function,result}` with raw ABI codes
  classified into a bounded `result` label set (cardinality budget, never
  raw codes), `ncgo_plugin_host_call_duration_seconds{plugin,function}`
  with fixed µs-to-seconds buckets, and
  `ncgo_plugin_capability_denials_total{plugin,function}` as the security
  signal. Instrumentation lives at the single choke point every ~39 host
  function passes through (the `export` helper in `registerHostModule`),
  via one `reflect.MakeFunc` wrapper shape-validated at startup; a nil
  registry leaves functions fully unwrapped (zero overhead). New
  `observability.metrics_enabled` (default false) and
  `observability.metrics_token` config gate `GET /metrics` on the main
  listener, with optional constant-time-compared bearer auth. OTel spans,
  the per-plugin admin dashboard, memory high-water marks, and guest
  entry-point metrics are deferred (documented in ADR-0055).
- **2026-09-22** — Phase 4h: static serving for the Nextcloud admin frontend
  (ADR-0054). New `web.static_root` config (empty = disabled, absolute path
  validated) points ncgo at an operator-provided directory of pre-compiled
  frontend assets — a Nextcloud release web root or a built apps dir; ncgo
  deliberately does not build, bundle, or rewrite the Vue frontend. The
  handler (`internal/web.StaticUI`) mounts as a GET/HEAD catch-all on `/`
  underneath every existing exact/prefix route (router longest-prefix wins),
  serves files with containment guarantees (`..` segments, prefix checks,
  per-request symlink resolution all enforced; canary tests prove nothing
  outside the root is reachable), an explicit extension→MIME allowlist plus
  the base chain's `nosniff`, immutable year-long caching for content-hashed
  bundle names and `no-cache` for the shell, `Last-Modified`/
  `If-Modified-Since` via `http.ServeContent`, and SPA fallback: extensionless
  misses serve `index.html` with 200 while misses with an extension and
  directories without an index 404. No CSRF weakening — static GETs are safe
  methods; the unimplemented `requesttoken` flow means SPA form POSTs fail
  412 in v1, with login v2 and Basic/app-password as the supported entry
  points. API coverage is honest: frontend calls beyond Phases 1–3 surface
  as frontend errors, not server crashes. Deferred: precompressed
  gzip/brotli sidecars, requesttoken endpoint, embedded minimal admin
  console, server-rendered bootstrap state.

- **2026-09-22** — Phase 4g: server-side image preview generation
  (ADR-0053). Authenticated `GET /index.php/core/preview[.png]` serves
  aspect-preserving fit-box previews of JPEG/PNG/GIF files (GIF first frame
  → PNG), gated by `files.DAV.Read` so preview access equals read access
  including shares, with auth via the same `webdav.Auth` chain as DAV routes
  (session cookies, app passwords, Bearer). Content is sniffed
  (`http.DetectContentType` on the first 512 bytes — extensions never
  trusted), `DecodeConfig` rejects sources outside 1..8192 per edge before
  any pixel decoding (decompression-bomb guard), source reads cap at
  256 MiB, and every failure — non-image, corrupt, E2EE/encrypted blob,
  missing file — is a uniform 404 with no fallback to source bytes.
  Previews are cached in the configured storage backend under
  `appdata_<instanceID>/previews/` keyed by
  sha256(uid+path+etag+box), so rewrites invalidate implicitly; misses
  generate under singleflight (one decode per concurrent burst), scale with
  `golang.org/x/image/draw.ApproxBiLinear` (approved dependency exception;
  `x/sync` promoted from indirect), and encode JPEG q85 for JPEG sources
  else PNG (re-encoding strips EXIF). New config: `previews.enabled`
  (default true), `previews.max_dimension` (default 2048, bounds 32..4096).
  Deferred: document previews via external service, fill/crop modes,
  pre-generation cron, cache GC, WebP.

- **2026-09-22** — Phase 4f: transparent server-side encryption at rest
  (ADR-0052). A `storage.Storage` decorator (`internal/storage/encrypt`)
  seals file contents with chunked AES-256-GCM: 64 KiB plaintext chunks, a
  40-byte header (magic `NCGOENC1` + 32-byte random salt), a per-file data
  key derived as HMAC-SHA256(masterKey, salt), and per-chunk counter
  nonces — stdlib crypto only, no new dependencies. Reads auto-detect the
  magic, so encrypted files decrypt transparently while legacy plaintext
  passes through untouched: enabling encryption on an existing instance is
  mixed-state by design, and old files are sealed as they are rewritten
  (DAV writes always stream through `Storage.Create`). Stat/List report
  plaintext sizes so the filecache stays consistent; every chunk read
  verifies the GCM tag and tamper/wrong-key/truncation fail loudly
  (`ErrIntegrity`). The master key is 32 random bytes, base64 in an
  operator-created 0600 key file from `encryption.master_key_path`
  (validated when `encryption.enabled`); the server fails closed at
  startup on key problems and never generates or logs key material.
  `ncgo-cli encryption init` writes the key file (refuses overwrite
  without `--force`) and `encryption status` reports enabled state plus
  key health. Threat model: protects against backend compromise, not
  server compromise; names/dirs/sizes stay plaintext in v1; E2EE folders
  remain opaque pass-through blobs with no server-side processing
  guarantees. Follow-ups: key rotation, per-user keys, encrypt-all sweep,
  filename encryption, SSE-C.
- **2026-09-22** — Phase 4e4: `ncgo-cli import-nextcloud dav` imports
  calendars, calendar objects, address books, and cards from a PHP Nextcloud
  database (`oc_calendars`/`oc_calendarobjects`/`oc_addressbooks`/`oc_cards`)
  — **completing the 4e import-nextcloud series** (users → files → shares →
  dav) (ADR-0051). Objects are written through the production
  `PutObject` store path, so UID/component/occurrence indexes, size, and
  etag (SHA-1 of the verbatim ICS/vCard bytes) are computed exactly as a
  client PUT computes them; Nextcloud's pre-computed columns are not
  trusted. etags, synctokens, and lastmodified are regenerated (ncgo assigns
  fresh ctags, bumped per object) — DAV clients must do one full resync
  after migration, the accepted trade-off of the series. Calendar
  color/order/timezone/displayname/description are preserved; the
  per-calendar components list and transparent flag are dropped with summary
  warnings. Calendar sharing (`oc_calendarshares`/`oc_dav_shares`) is
  deferred with a counted warning (invite-state mapping is out of scope;
  re-share after migration). Principals must be `principals/users/<uid>`
  with the user already in the target; unknown owners, non-user principals,
  empty object data, and owner+uri collisions with differently-propertied
  collections are explicit skips (collisions skip the source collection
  wholesale). Idempotent and resumable: equivalent existing collections are
  skipped while missing objects still import, so an interrupted run can
  simply be repeated; `--dry-run` writes nothing.
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
