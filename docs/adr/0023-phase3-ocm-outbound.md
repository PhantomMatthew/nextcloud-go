# ADR-0023: Phase 3d2 Outbound OCM

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3d inbound OCM is on local main (`3406b51`). Recipients can receive
`POST /ocm/shares`, but a local client cannot federate a local file out:
`sharing.Create` rejected `shareType=6`. Outbound rows have a local owner and
filecache path, unlike inbound placeholders.

## Decision

1. **OCS `shareType=6`.** `files.ShareTypeRemote = 6`. Outbound rows reuse
   `shares` (`share_with` is the cloud ID). No new migration.
2. **Discover then notify.** After inserting the local row, `ocm.Client`
   `GET {origin}/.well-known/ocm` (fallback `/ocm-provider`) then
   `POST {endPoint}/shares`. HTTP is injectable (`Timeout=15s`). Capture and
   replay stub only `remote.example.com`.
3. **Rollback.** Missing `@` or empty remote → OCS 400 unknown sharee.
   Self-federation (remote host equals request Host) → same. Discover or
   notify failure deletes the local row → OCS 400 `cannot federate share`.
4. **No capability.** `files_sharing.federation` stays false. Capabilities
   goldens are not recaptured.
5. **No DAV proxy.** Inbound remote GET remains 501. Delete outbound is
   local-only (no `POST /ocm/notifications` unshare).

## Alternatives Considered

### New outbound table
- Pros: symmetric with `ocm_incoming`.
- Cons: owner and `file_path` already fit `shares`.

### Flip federation capability now
- Pros: desktop UI can offer federated share.
- Cons: recapture capabilities; this slice only needs OCS create.

### Proxy inbound WebDAV now
- Pros: GET works for incoming mounts.
- Cons: TLS, tokens, and Range; locked out of this increment.

## Consequences

### Positive
- A local owner can federate `/hello.txt` to `bob@https://remote.example.com`.

### Negative
- Opening an incoming federated file is still 501. Desktop may hide the
  share action until capability is true.

### Neutral / follow-ups
- `DefaultSharingProvider.Federation=true`, inbound DAV proxy, unshare
  notifications, HTTP Message Signatures, invites, federated user search.

## References

- Open Cloud Mesh API
- ADR-0022
- `docs/plans/00-phased-rewrite-plan.md` Phase 3
