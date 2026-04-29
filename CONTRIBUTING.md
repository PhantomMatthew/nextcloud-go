# Contributing to nextcloud-go

Thanks for your interest. This project is a **greenfield Go reimplementation**
of [Nextcloud Server](https://github.com/nextcloud/server), wire-compatible with
the official desktop, iOS, Android, CalDAV, and CardDAV clients.

The project is in **Phase 0** (foundation). Most of the surface area does not
exist yet. See [`docs/`](./docs/) for the planning corpus before contributing.

## Ground rules

1. **Read the planning docs first** — [`docs/plans/00-phased-rewrite-plan.md`](./docs/plans/00-phased-rewrite-plan.md)
   and [`docs/plans/01-phase-0-blueprint.md`](./docs/plans/01-phase-0-blueprint.md).
2. **Architecture decisions are recorded as ADRs** — see
   [`docs/adr/`](./docs/adr/). Significant changes require a new ADR.
3. **Wire compatibility is non-negotiable** — every endpoint behavior must be
   verified against the golden-test harness (`testdata/golden/`).
4. **No CGO** unless explicitly approved via ADR. See ADR-0001.
5. **License: AGPL-3.0-or-later.** All contributions are submitted under the
   project license. By opening a PR you certify the [Developer Certificate of
   Origin](https://developercertificate.org/).

## Development setup

```bash
# Toolchain
go version             # 1.22 or 1.23
golangci-lint --version
docker --version

# Build & test
make build
make test
make lint

# Local stack (postgres + redis + ncgo)
make dev-up
```

## Workflow

1. **Open an issue first** for non-trivial work. Cite the relevant ADR / plan.
2. **Branch naming**: `phase-N/<short-slug>` or `fix/<slug>`.
3. **Commits**: imperative, scoped, one logical change per commit.
   Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`,
   `chore:`) preferred.
4. **PRs**: must pass `make all` (fmt + vet + lint + test + build) and CI.
   Include rationale, link to ADR/plan section, and golden-test additions
   when behavior changes.

## Code style

- `gofumpt` formatting (enforced via `golangci-lint`).
- `goimports` with local prefix `github.com/PhantomMatthew/nextcloud-go`.
- Structured logging via `slog` only — no `fmt.Print*` in library code.
- Errors: wrap with `%w`, never swallow, never `panic` in library code.
- Tests: table-driven, `testify/require` for assertions, `testcontainers-go`
  for integration tests.

## Reporting security issues

Do **not** open a public issue. Email security@TBD or use GitHub's private
security advisory feature once enabled.

## Code of Conduct

See [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md). Be kind. We're rewriting
~500K LOC of PHP — patience is mandatory.
