# nextcloud-go

A ground-up Go rewrite of [Nextcloud Server](https://github.com/nextcloud/server),
wire-compatible with existing Nextcloud desktop, iOS, Android, CalDAV, and CardDAV
clients.

> **Status**: Phase 0 — foundations. `ncgo serve --dev` boots an in-memory
> SQLite server that serves `/status.php`, OCS capabilities, login v2, and a
> WebDAV stub.

## Quick start

```bash
go run ./cmd/ncgo serve --dev
# in another terminal
curl -sS http://127.0.0.1:8080/status.php
go run ./cmd/ncgo-captest run --cases testdata/golden
```

`serve --dev` uses shared-memory SQLite, bootstrap user `admin` / `admin`, and
text debug logs. For a file-backed config, copy the YAML schema in
`docs/plans/01-phase-0-blueprint.md` and pass `--config`.

### Serving the web frontend

ncgo serves — but does not build — the pre-compiled Nextcloud web UI. Point
`web.static_root` in the config at a directory of compiled frontend assets
(e.g. a Nextcloud release tarball's web root) and restart; with the default
empty value no static UI is served. Serving rules, cache policy, security
constraints, and the honest API-coverage caveats are in
[`docs/adr/0054-admin-ui-static-serving.md`](docs/adr/0054-admin-ui-static-serving.md).

### Managing plugins

`ncgo-cli plugin install` / `enable` / `disable` / `uninstall` update the
plugin registry in the database; the running server reads that set once at
boot, so every change takes effect only after a server restart.

## Project Goals

1. **Wire compatibility** — existing Nextcloud clients (desktop sync, iOS, Android,
   third-party CalDAV/CardDAV/WebDAV) connect to `nextcloud-go` without modification.
2. **Feature parity** with bundled Nextcloud apps over Phases 0–4 (~24 months).
3. **WASM plugin system** ([`wazero`](https://wazero.io)) for third-party extensions —
   sandboxed, capability-based, language-agnostic.
4. **Greenfield architecture** — fresh schema, no PHP coexistence, modern Go idioms.
5. **Operational simplicity** — single static binary, no CGO, container-friendly.

## Non-Goals (v1)

- Drop-in PHP replacement (greenfield schema; data migration tool comes in Phase 4)
- Loading existing PHP Nextcloud apps unmodified
- Web frontend rewrite (the existing JS/Vue frontend is reused; only the server changes)

## Repository Layout

```
cmd/                  Binaries (ncgo, ncgo-cli, ncgo-captest)
internal/             Private packages (app, config, database, cache, storage, auth,
                      httpx, ocs, webdav, jobs, plugins, observability)
pkg/                  Public stable contracts (api, pluginsdk)
deploy/               docker
docs/                 architecture, adr, plans, specs
tools/                capture, golden-gen
```

## Documentation Index

- [`docs/plans/00-phased-rewrite-plan.md`](docs/plans/00-phased-rewrite-plan.md) — full Phases 0–4 roadmap
- [`docs/plans/01-phase-0-blueprint.md`](docs/plans/01-phase-0-blueprint.md) — detailed Phase 0 implementation plan
- [`docs/specs/wasm-plugin-abi.md`](docs/specs/wasm-plugin-abi.md) — WASM plugin ABI specification
- [`docs/architecture/overview.md`](docs/architecture/overview.md) — system architecture overview
- [`docs/adr/`](docs/adr) — Architecture Decision Records

## License

**AGPL-3.0-or-later**, matching upstream Nextcloud.
