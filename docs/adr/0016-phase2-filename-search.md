# ADR-0016: Phase 2i Filename Search

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2i unified-search decisions relative to ADR-0015)

## Context

Phase 2 requires server-side search via the OCS Search API. Desktop and mobile
clients list providers then query `files` by term. Content indexing, extra
providers, and federated search stay later.

## Decision

1. **No new table.** Search reads the live `files` filecache. Trash and
   versions are not queried.
2. **OCS routes.** `GET /ocs/v{1,2}.php/search/providers` and
   `GET /ocs/v{1,2}.php/search/providers/files/search?term=` via `HandlePrefix`.
   Auth matches `cloud/user`. Unknown provider ids are OCS 404. Empty terms
   return an empty `entries` list.
3. **One provider.** `id=files`, `name=Files`, `order=5`.
4. **Basename match.** `files.Store.SearchByName` matches `files.name` for the
   current user only. Postgres uses `ILIKE`; MySQL and SQLite use `LIKE`.
   `%`, `_`, and `\` are escaped. Default and maximum size is 20. The user
   root (`path=/`) is excluded.
5. **Hit fields.** `title` is the basename, `subline` is the path,
   `resourceUrl` is `{scheme}://{host}/index.php/f/{id}` (https unless the
   host is localhost). `thumbnailUrl` and `icon` are empty; `rounded` is false.
6. **No capability block.** Clients call the OCS routes directly.

## Alternatives Considered

### Full-text / content index
- Pros: matches file bodies.
- Cons: needs a new store, jobs, and extraction pipeline.

### Search only `files.path`
- Pros: one column.
- Cons: a term can match parent folders; basename `name` is what the unified
  search UI shows as the title.

## Consequences

### Positive
- Clients can list the files provider and find `/hello.txt` by name.

### Negative
- Only basename substring match; no filters, pagination cursor, or previews.

### Neutral / follow-ups
- User/group shares and files_lock OCS leftovers remain later Phase 2
  increments.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- ADR-0015
