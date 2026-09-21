# ADR-0036: Phase 3f3 Sharees Recommended Recipients

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Since Phase 3d5 (ADR-0026), any sharees path remainder other than `/`
was OCS 404, including `/recommended`. The desktop share dialog queries
`/recommended` to suggest recipients.

## Decision

1. **Recommend from current shares.** `GET sharees/recommended` returns
   the distinct local user/group recipients (`shareType` 0/1) of the
   caller's current shares, most recent first (`stime` desc), capped at
   20 per collection. No circles engine, no history of deleted shares.

2. **Reuse, no new store method.** Recipients come from the existing
   `files.ShareStore.ListByOwner`, deduplicated in the handler. Labels
   resolve display names via `users.Store` (`GetByUID` /
   `GetGroupByGID`). `ShareesHandler` gains a `Shares files.ShareStore`
   field; nil disables recommendations (hermetic unit tests).

3. **Envelope.** Same sharees data shape as the search endpoint:
   `users`/`groups` filled, everything else empty arrays.

4. **Goldens.** 017 creates a group share for `engineers`; 018
   `/recommended` returns it (the bob user share from 007 was deleted
   by 009 and must not appear).

## Alternatives Considered

### Recommendation from share history including deleted shares
- Pros: closer to PHP's "you shared with them before".
- Cons: needs soft-deleted or logged shares; the shares table is
  hard-delete.

## Consequences

### Positive
- The share dialog can suggest real recipients derived from actual
  sharing behavior.

### Negative
- A brand-new user gets an empty recommendation list until they share
  something.

### Neutral / follow-ups
- Circles-based recommendations, emails collection.

## References

- nextcloud/server `apps/files_sharing` ShareesController recommended
