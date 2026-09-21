# ADR-0029: Phase 3d8 Lookup Server Federated User Search

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3d5 (ADR-0026) added OCS sharees with `exact.remotes` for an exact
cloud ID, but ignored the `lookup` parameter and left the `lookup`
collection empty. Desktop and web share dialogs send `lookup=true` to
search the global directory when the typed text is not a cloud ID, so
federated user search by name was impossible.

## Decision

1. **Query side only.** `sharing.LookupClient` issues
   `GET {BaseURL}/users?search={query}` with `Accept: application/json`
   and a 10s default timeout, parses the lookup server JSON array, and
   keeps only `federationId` and `name`. Hosting a lookup server is out
   of scope.

2. **Opt-in per request.** The sharees handler queries the lookup server
   only when `lookup=true|1` and `search` is non-empty. The capability
   `files_sharing.sharee.query_lookup_default` stays `false` (PHP
   default); clients opt in explicitly.

3. **Silent degradation.** Lookup server errors (non-2xx, timeout,
   decode failure) never fail the OCS request; the `lookup` collection
   is just empty. Hits that are not valid cloud IDs or point at the
   request host are dropped, same as `exact.remotes`.

4. **Configurable.** `sharing.lookup_server` defaults to
   `https://lookup.nextcloud.com`; empty disables lookup entirely.

5. **Goldens.** Add sharing 013 (lookup hit) and 014 (lookup miss). The
   replay `remoteOCMRoundTripper` answers `lookup.nextcloud.com`; no
   recapture of 011/012.

## Alternatives Considered

### Always query the lookup server
- Pros: name search works without a client flag.
- Cons: leaks every typed name to an external service; diverges from
  PHP where `lookup` is an explicit parameter.

### Bundle local users/groups typeahead
- Pros: one increment fills more sharees collections.
- Cons: separate concern; locked out of this increment.

## Consequences

### Positive
- Federated user search by name works for any user registered on the
  configured lookup server.

### Negative
- Each `lookup=true` request is a synchronous external HTTP call on the
  request path (bounded by the 10s client timeout).

### Neutral / follow-ups
- sharees/recommended, local users/groups typeahead, CalDAV leftovers.

## References

- nextcloud/lookup-server search API (`GET /users?search=`)
- nextcloud/server `apps/files_sharing` ShareesController
