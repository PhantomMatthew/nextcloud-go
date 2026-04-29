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
- CI matrix green on Linux/macOS × Go 1.22/1.23

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
- Reference plugins: file tagger, simple webhook, OAuth provider
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

- **2026-04-29** — Initial plan committed.
