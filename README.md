# nextcloud-go

A ground-up Go rewrite of [Nextcloud Server](https://github.com/nextcloud/server),
wire-compatible with existing Nextcloud desktop, iOS, Android, CalDAV, and CardDAV
clients.

> **Status**: Phase 0 — Planning & Foundations.
> No runnable code yet. See [`docs/`](./docs) for architecture, ADRs, and the phased plan.

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

## Repository Layout (Planned — Phase 0)

```
cmd/                  Binaries (ncgo, ncgo-cli, ncgo-captest)
internal/             Private packages (app, config, db, cache, storage, auth,
                      httpx, ocs, webdav, jobs, eventbus, plugin, obs)
pkg/                  Public stable contracts (api, pluginsdk)
modules/              First-party in-binary feature modules
test/                 compat, golden, fixtures, integration, e2e
deploy/               docker, helm, systemd
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

Will be **AGPL-3.0-or-later**, matching upstream Nextcloud, when code is committed.
