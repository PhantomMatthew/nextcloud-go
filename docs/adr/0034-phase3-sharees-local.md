# ADR-0034: Phase 3f1 Sharees Local User/Group Typeahead

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3d5/3d8 sharees resolved exact federated cloud IDs and lookup
server hits, but the local `users`/`groups` collections stayed empty
(ADR-0026 follow-up). The desktop share dialog could not complete local
user or group names.

## Decision

1. **Store search.** `users.Store` gains `Search` (enabled users, uid
   or display name substring, case-insensitive, cap 20) and
   `SearchGroups` (gid or display name substring), using `ILIKE` on
   Postgres and `LIKE` elsewhere, matching the files `SearchByName`
   idiom.

2. **Payload rules.** `ShareesHandler` gains a `Users users.Store`
   field (nil disables local search, keeping unit tests hermetic). An
   exact uid/gid match lands in `exact.users`/`exact.groups`; other
   matches land in `users`/`groups`. Labels prefer display name.
   shareType 0 for users, 1 for groups.

3. **Goldens.** 012 recaptured and renamed to
   `012-sharees-local-exact` (search=bob now hits a local user); 013
   recaptured (lookup hit plus local exact user); new 015 (partial user
   typeahead) and 016 (group match). Seed gains an `engineers` group
   with bob as member.

## Alternatives Considered

### Reuse the unified-search provider (Phase 2i)
- Pros: one search surface.
- Cons: unified search covers files only; sharees needs principal
  search with OCS-specific shaping.

## Consequences

### Positive
- Local share recipients are discoverable from the standard sharees
  endpoint for users and groups.

### Negative
- Substring search is unindexed (`LIKE '%term%'`); acceptable at the
  user counts of a single-node v1.

### Neutral / follow-ups
- sharees/recommended, emails/circles collections.

## References

- nextcloud/server `apps/files_sharing` ShareesController local search
