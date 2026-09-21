# ADR-0035: Phase 3f2 CardDAV Contact Groups

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 3 in scope lists CardDAV "address books, contacts, groups".
`parseVCard` (Phase 3b, ADR-0020) accepts any single VCARD with a UID,
so group vCards already round-trip; this increment proves and freezes
that behavior.

## Decision

1. **Groups are data, not protocol.** A contact group is a vCard with
   `KIND:group` and `MEMBER` properties (RFC 6350 §6.1.4); category
   groups are `CATEGORIES` on regular contacts. Both are plain VCARD
   payloads — the CardDAV store needs no group-specific handling, so
   none is added.

2. **Proof by test.** `TestContactGroup` writes a `KIND:group` vCard
   with a MEMBER line, reads it back byte-identical, and confirms
   `addressbook-query` returns it. Goldens carddav 010 (PUT group) and
   011 (GET group) freeze the wire surface.

## Alternatives Considered

### Server-side group membership resolution
- Pros: enables group sharees and group ACL expansion.
- Cons: requires parsing MEMBER/CATEGORIES into tables; out of scope
  until a consumer exists (e.g. circles).

## Consequences

### Positive
- iOS/macOS Contacts and Nextcloud clients can sync group vCards
  without any server change.

### Negative
- The server cannot answer "which contacts are in group X" without a
  client-side vCard scan.

### Neutral / follow-ups
- Membership indexing if circles/group sharing lands.

## References

- RFC 6350 §6.1.4 (KIND:group), RFC 6352 (CardDAV)
