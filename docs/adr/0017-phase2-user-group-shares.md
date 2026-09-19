# ADR-0017: Phase 2k User and Group Shares

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2k decisions relative to ADR-0013 / ADR-0016)

## Context

Phase 2f delivered public links (`shareType=3`). Desktop and mobile clients also
create user (`0`) and group (`1`) shares and expect the sharee to see the item
inside `/remote.php/dav/files/{sharee}/`. Reshare, federation, and an extra
incoming mount stay later.

## Decision

1. **Types.** OCS `shareType` `0` and `1` are accepted. Type `6` and any other
   value remain OCS 400 (`unknown share type`). Missing or unknown `shareWith`
   is OCS 400 (`unknown sharee`).
2. **Schema.** Migration `0009_share_recipients` adds `shares.share_with`
   (TEXT / VARCHAR(255), empty for public links) and `accepted` (INT, default 1).
   Path remains the owner filecache key. No FK to `files`.
3. **Store.** `files.Share` gains `ShareWith` and `Accepted`. `ShareStore` adds
   `ListBySharee(shareWith, groupGIDs)`. Groups use the existing `groups` /
   `group_members` tables via `users.Store`.
4. **Permissions.** Default is read (`1`). Folders may also have create /
   update / delete. The reshare bit is rejected.
5. **OCS payload.** User/group rows fill `share_with`, `share_with_displayname`,
   and `uid_file_owner`. `url` is empty (public links still emit `/s/{token}`).
6. **Recipient DAV.** Shares appear in the sharee's files jail root under the
   owner path's basename (`Shared=true`, `Shareable=false`). `Stat` / `Read` /
   `Write` / `List` resolve to the owner's `file_path` plus any relative
   suffix. Permission bits gate writes. Recipient `MOVE` of an incoming path
   is 403. `COPY` does not copy share rows. Owner `MOVE` still `RenamePath`.
7. **Capabilities.** `files_sharing.user` and `group_sharing` are true;
   `resharing` and `federation` stay false.
8. **Public tokens.** `LookupValid` still requires `shareType=3`, so a user
   share token is not a public link.

## Alternatives Considered

### Separate `/remote.php/dav/shared/` mount
- Pros: no basename collisions with the sharee's own files.
- Cons: clients already list incoming shares in the home jail.

### Pending `accepted=0` workflow
- Pros: matches Nextcloud accept/reject mail shares.
- Cons: out of this increment; default is accept.

## Consequences

### Positive
- A second user can receive `/hello.txt` and PROPFIND it after an OCS create.

### Negative
- Two incoming shares with the same basename: the owner's own file wins.
- Group membership is SQL-only; there is no OCS group admin API yet.

### Neutral / follow-ups
- Reshare, federation, and files_lock OCS leftovers remain later.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- ADR-0013
