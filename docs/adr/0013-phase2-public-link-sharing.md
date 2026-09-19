# ADR-0013: Phase 2f Public Link Sharing

- **Status**: Accepted
- **Date**: 2026-09-19
- **Deciders**: Project lead
- **Supersedes**: (none; records Phase 2f public-link decisions relative to ADR-0012)

## Context

Desktop clients create public links through OCS `files_sharing` (`shareType=3`)
and open them via `/public.php/webdav` and `/s/{token}`. User/group shares,
reshare, federation, and the Vue public HTML page stay later.

## Decision

1. **Public links only.** POST with any `shareType` other than `3` is OCS 400.
   No incoming-share DAV and no share-with user.
2. **Path-keyed store.** Migration `0008_shares` table `shares` is unique on
   `token`, with `owner_user_id` FK to `users` ON DELETE CASCADE and no FK to
   `files`. `expire_ms` `0` means none. Lazy expire on OCS get and public access.
3. **Tokens.** Fifteen `[a-z0-9]` characters. Tests and goldens freeze
   `ncgopublic00001`.
4. **OCS.** `/ocs/v{1,2}.php/apps/files_sharing/api/v1/shares` via `HandlePrefix`.
   Default permissions are read (`1`). Folder shares may add create/update/delete.
   Password is optional Argon2id. Share URL is `{scheme}://{host}/s/{token}`.
5. **Public DAV.** `/public.php/webdav` jails over the owner's `files.DAV` at
   `file_path`. Reads always succeed when the share is valid. PUT/MKCOL/DELETE
   require permission bits. MOVE/COPY/PROPPATCH/LOCK are 405. Writes call
   `CheckLock` on the owner path. Entries are `Shared=true`, `Shareable=false`.
6. **Public GET.** `GET /s/{token}` returns file bytes. Folders return 401 (DAV,
   not Vue). Missing or expired tokens return 404.
7. **Lifecycle.** Files MOVE renames share rows. COPY does not copy shares.
   Trash and Purge delete shares at the files path.
8. **Capabilities.** `files_sharing` advertises `api_enabled` and public
   upload; password is not enforced; user/group/resharing/federation are false.

## Alternatives Considered

### Full share graph in this increment
- Pros: closer to Nextcloud desktop share dialogs.
- Cons: user/group/reshare/federation multiply the auth and DAV surface.

### `/remote.php/dav/public-files/`
- Pros: newer DAV namespace.
- Cons: desktop still uses `/public.php/webdav` for public links.

## Consequences

### Positive
- Public links work for OCS create/list/delete and anonymous file GET.

### Negative
- Password-protected folder browsing has no HTML UI.

### Neutral / follow-ups
- User/group shares, reshare, federation, Vue public pages, S3, jobs, and
  search remain later Phase 2/3 increments.

## References

- `docs/plans/00-phased-rewrite-plan.md` Phase 2
- ADR-0012
