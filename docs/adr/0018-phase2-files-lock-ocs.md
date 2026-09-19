# ADR-0018: Phase 2e2 files_lock OCS and Depth Infinity

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none; records 2e leftovers relative to ADR-0012 / ADR-0017)

## Context

Phase 2e delivered exclusive write LOCK/UNLOCK on files DAV. Clients also call
the files_lock OCS API by numeric fileid, and RFC 4918 Depth `infinity` should
cover members of an existing collection. Shared locks and lock-null stay out.

## Decision

1. **OCS routes.** `PUT` and `DELETE` on
   `/ocs/v{1,2}.php/apps/files_lock/lock/{fileid}` via `HandlePrefix`. Auth
   matches `cloud/user`. The handler resolves the fileid to an owner path and
   calls `DAV.Lock` / `DAV.Unlock`. Missing or foreign fileids are OCS 404.
   Already-locked files are OCS/HTTP 423 on v2.
2. **No new table.** Locks stay in `file_locks`. Token uniqueness is unchanged,
   so Depth infinity does not insert one row per child.
3. **Depth infinity.** WebDAV `Depth: infinity` is accepted. `CheckLock` walks
   from the target path to `/` and treats an ancestor lock as covering the
   member. Missing paths still 404. There is no lock-null resource.
4. **Shared locks.** `<d:shared/>` remains 403.
5. **Unlock token.** OCS DELETE reads `Lock-Token` or the `token` query.
6. **No new capability block.** Clients call the OCS routes directly.

## Alternatives Considered

### Child lock rows with derived tokens
- Pros: `GetByPath` on a child finds a row.
- Cons: `token` is UNIQUE; one lock cannot occupy many rows.

### Depth column on `file_locks`
- Pros: Depth 0 on a collection would not cover members.
- Cons: needs a migration; 2e2 treats ancestor locks as covering members.

## Consequences

### Positive
- Desktop can lock `hello.txt` by fileid and see 423 on a tokenless PUT.
- A collection LOCK covers existing children via ancestor walk.

### Negative
- A Depth 0 LOCK on a directory also covers children (same walk).

### Neutral / follow-ups
- Shared locks, lock-null, and distributed lock managers stay later.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- ADR-0012
