# ADR-0006: Phase 0 Toolchain and Scope Adjustments

- **Status**: 🟢 Accepted
- **Date**: 2026-09-17
- **Deciders**: Project lead
- **Supersedes**: (none; records deviations from ADR-0001, ADR-0003, and the Phase 0/1 blueprints)

## Context

Closing Phase 0 required choices that were left open or that later evidence
overturned. This ADR records those adjustments so later phases do not re-litigate
them from comments in blueprints.

## Decision

1. **Go version.** The module and CI track the current Go 1.27 stable line
   (`go 1.27.1` in `go.mod`, `go-version-file: go.mod` in CI). The OS matrix is
   Linux and macOS; the Go version is no longer a matrix axis.
2. **TOML.** Plugin manifests use `github.com/pelletier/go-toml/v2`. Server
   configuration remains YAML via koanf.
3. **Timestamps.** Schema `0001_init` stores instants as `BIGINT` Unix
   milliseconds on all three dialects instead of `TIMESTAMPTZ`, so scanning is
   identical across `database/sql` drivers.
4. **Fuel metering.** wazero v1 has no fuel API. The Phase 0 plugin stub enforces
   memory page limits and wall-clock timeouts (`WithCloseOnContextDone`) only.
   Fuel remains a Phase 4 item.
5. **Integration tests.** `testcontainers-go` is deferred. Postgres, MySQL, and
   Redis tests use `//go:build integration` and `NCGO_TEST_*` DSNs; CI provides
   service containers. SQLite tests always run.
6. **Database access.** All SQL goes through `database/sql` (pgx stdlib, MySQL,
   modernc SQLite) with `?` placeholders rebound to `$n` for Postgres.
7. **`pkg/api`.** Phase 0 freezes only `Module`, `Route`, `ModuleHost`, and
   `Host`. DB/storage/cache/jobs interfaces live in their `internal/` packages.
   The architecture rule that in-tree modules talk only through `pkg/api` is
   postponed.

## Alternatives Considered

### Pin Go 1.22/1.23 as in the original CI matrix
- Pros: matches the earliest blueprints.
- Cons: golangci-lint v1 cannot analyze Go 1.27; the approved toolchain is 1.27.

### Keep TIMESTAMPTZ
- Pros: closer to the blueprint DDL.
- Cons: driver-specific scan types; BIGINT is portable.

### Ship wazero fuel in Phase 0
- Pros: matches ABI spec `fuel_per_call`.
- Cons: not available in wazero v1.

## Consequences

### Positive
- One documented place for Phase 0 deviations.
- CI and local toolchain stay aligned on Go 1.27 and golangci-lint v2.

### Negative
- Blueprints still mention chi, prometheus, otel, and testcontainers as future
  work; those remain unimplemented in Phase 0.

### Neutral / follow-ups
- Revisit fuel metering and `pkg/api` as the sole module boundary in Phase 4.
- Revisit testcontainers if local integration UX becomes a bottleneck.

## References

- [ADR-0001](../adr/0001-tech-stack.md)
- [ADR-0003](../adr/0003-wasm-plugin-system.md)
- [Phase 0 blueprint](../plans/01-phase-0-blueprint.md)
- [WASM plugin ABI](../specs/wasm-plugin-abi.md)
