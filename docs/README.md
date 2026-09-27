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

## Operations

### Filename encryption (ADR-0104)

NCGOFN1 deterministic name tokens keyed by per-user directory keys. Opt-in;
requires the per-user key hierarchy.

1. **Enable**: set `encryption.enabled: true`, `encryption.per_user_keys: true`,
   and `encryption.filename_encryption: true`, then restart. Users created from
   now on start encrypted; existing trees stay plaintext until swept.
2. **Sweep**: `ncgo-cli encryption encrypt-names [--user uid] [--dry-run]`.
   Per-user, one DB transaction each; idempotent (scheme-1 users skip). Trash
   objects move to token-based location ids; locks/versions/trash/share rows
   whose paths no longer resolve are skipped and counted (retention self-heals
   them). Run `--dry-run` first for the per-user report.
3. **Verify**: `ncgo-cli encryption status` prints `filename encryption:
   N/M users scheme-1, K tokenized rows`; a `scheme-1 folders WITHOUT a
   directory key` warning means a sweep missed folders (their children's names
   are unresolvable) — re-run `encrypt-names`; the count exits nonzero.
4. **Enrolled users** (password-wrapped keys, ADR-0100) are never converted by
   the offline CLI — their key boxes open only in an unlocked session. They
   convert **automatically at their next password login** (the server's login
   hook runs the sweep holding the unlocked key; best-effort, never fails the
   login). The same hook closes the bootstrap-admin gap: an admin created
   before the wiring converts at first login.
5. **Limits**: 255 runes per name, 768 chars per computed ciphertext path
   (enforced on every dialect; over-budget creates/renames fail with `400`).
   A sweep hitting an over-budget existing name aborts that user with nothing
   written — rename the file, then re-run.
6. **Rollback/decommission**: `ncgo-cli encryption decrypt-names [--user uid]`
   restores plaintext names per user (same transaction shape; folder key UUIDs
   and their wrap rows are stripped; file content stays sealed). Enrolled
   users must **unenroll first** (they log in once with
   `encryption.password_wrapped_keys` off). An older binary cannot resolve
   scheme-1 paths — never roll back the binary before `decrypt-names`
   completes. In-flight chunked uploads spanning a sweep fail loudly at
   finalize and retry cleanly; run sweeps quiesced if that matters.

### appdata and previews under per-user encryption (ADR-0105)

`encryption.per_user_keys` covers system trees too; nothing to enable
beyond the mode itself.

- **appdata content is master-sealed in every mode.** The preview cache and
  plugin system storage under `appdata_<instanceID>/` seal under the current
  keyring key (v2 fallback; v1 on a single-key ring) instead of failing with
  "user not found". `ncgo-cli encryption rotate-keys` covers these blobs —
  the sweep walks from the storage root.
- **Previews of per-user (v3) files self-seal under the source file's key**
  (NCGOPV1) and live in a separate `appdata_<id>/previews_enc/` prefix,
  written and read via the raw backend. Serving resolves the file key in the
  requester's ctx — owner session, sharee wrap, or master-mode anonymous —
  so a locked enrolled file's preview 404s exactly like its content.
  Previews of non-v3 files keep the legacy decorated `previews/` path, and
  pre-existing cache entries keep working.
- **Enrolled users** (password-wrapped keys, ADR-0100): the upload-time
  pregeneration job cannot resolve their file keys (principal-less ctx) and
  skips those files (debug-logged, never an error) — the first interactive
  preview request pays the render. Master-wrapped users pregenerate
  normally.
- **Rollback**: an older binary never sees `previews_enc/` (clean cache
  miss → regenerate into `previews/`); the cache is self-healing. Preview GC
  sweeps both prefixes.

## Status Legend

- 🟢 **Accepted** — current authoritative design
- 🟡 **Draft** — under active design, may change
- 🔴 **Superseded** — historical only, see linked successor
- ⚪ **Proposed** — not yet decided
