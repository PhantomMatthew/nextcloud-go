# Documentation Index

This directory contains all design and planning artifacts for the `nextcloud-go` rewrite.

## Structure

| Directory | Purpose |
|---|---|
| [`architecture/`](architecture) | System architecture overviews, component diagrams, subsystem deep-dives |
| [`adr/`](adr) | Architecture Decision Records — immutable, numbered, append-only |
| [`plans/`](plans) | Project plans (phased roadmap, per-phase blueprints, milestones) |
| [`specs/`](specs) | Detailed specifications (protocols, ABIs, file formats, schemas) |

## Reading Order

1. [`plans/00-phased-rewrite-plan.md`](plans/00-phased-rewrite-plan.md) — start here for the big picture
2. [`architecture/overview.md`](architecture/overview.md) — system architecture and core abstractions
3. [`plans/01-phase-0-blueprint.md`](plans/01-phase-0-blueprint.md) — what we're building right now
4. [`specs/wasm-plugin-abi.md`](specs/wasm-plugin-abi.md) — plugin system design (Phase 4 deliverable, designed in Phase 0)
5. [`adr/`](adr) — read all ADRs to understand why specific tools/approaches were chosen

## Conventions

- All documents are Markdown. No proprietary formats.
- ADRs are **immutable** once accepted. Supersede with new ADRs; never edit history.
- Plans and specs **are** living documents — update them as designs evolve, but record
  significant changes in a "Change Log" section at the bottom.
- Diagrams use Mermaid where possible (renders natively in GitHub).

## Status Legend

- 🟢 **Accepted** — current authoritative design
- 🟡 **Draft** — under active design, may change
- 🔴 **Superseded** — historical only, see linked successor
- ⚪ **Proposed** — not yet decided
