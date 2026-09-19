# ADR-0014: Phase 2h Background Jobs Runner

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2h jobs runner decisions relative to ADR-0013)

## Context

Phase 0 froze `internal/jobs.Runner` as an interface and created the `jobs`
table in `0001_init`. Nothing dequeued rows. Share and lock expiry still ran
lazily on access. Phase 2 requires a background job framework plus scheduled
jobs so expiry (and later indexing) can run without a request.

## Decision

1. **Reuse `0001_init.jobs`.** No new migration. Columns stay
   `id, name, payload, run_at, started_at, completed_at, last_error, attempts,
   created_at` (unix-ms).
2. **Single-node claim.** `SQLStore.ClaimDue` selects
   `started_at IS NULL AND completed_at IS NULL AND run_at <= now`, then
   updates `started_at` only when it is still NULL. There is no
   `FOR UPDATE SKIP LOCKED`.
3. **Complete vs fail.** Success writes `completed_at`. Failure increments
   `attempts`, stores `last_error`, pushes `run_at`, and clears `started_at`.
4. **`SQLRunner` implements `jobs.Runner`.** `App.New` calls `Start`; `Close`
   calls `Stop`. Workers and poll interval come from existing `jobs:` config
   (defaults 4 workers, 5s).
5. **Built-in jobs.** `shares.expire` deletes `shares` with
   `expire_ms > 0 AND expire_ms <= now`. `locks.expire` deletes `file_locks`
   with `timeout_ms > 0 AND timeout_ms <= now`. Lazy expire paths stay.
6. **Self-reschedule.** On `Start`, each registered periodic name is enqueued
   at `now+poll`. After a successful run the runner enqueues the same name
   again at `now+poll`.

## Alternatives Considered

### New jobs table / queue product
- Pros: retries, visibility timeout, multi-node.
- Cons: the Phase 0 table already exists; v1 is single-node.

### Drop lazy expire
- Pros: one code path.
- Cons: a stopped runner would leave expired shares and locks visible
  until the next poll.

## Consequences

### Positive
- Expired public links and write locks are removed without a user request.

### Negative
- A crash after claim and before complete leaves `started_at` set; that row
  is not reclaimed until process replacement logic is added later.

### Neutral / follow-ups
- S3, filename search, and user/group shares remain later Phase 2 increments.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- ADR-0013
