# ADR-0105: appdata system trees under per-user encryption — master-sealed fallback + per-file-key preview sealing

- **Status**: Accepted
- **Date**: 2026-09-27
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0097 (closes the documented gap "phase 1 does not seal
  system trees under per-user keys" — appdata writes failed loudly in
  per-user mode), ADR-0052 (the decorator gains one more write rule)

## Context

ADR-0097 made per-user (v3) sealing opt-in via `encryption.per_user_keys`.
The resolver's `Allocate` derives an owner from the storage key and **fails
loudly for ownerless keys**: everything under `appdata_<instanceID>/` —
today that is the preview cache (`…/previews`, ADR-0053) and plugin system
storage (`…/plugins`, ADR-0042/0061). Consequence: with per-user keys on,
preview generation and plugin storage writes error out ("user not found").
Verified facts that shape the design:

- Preview object keys are already pseudonymous: `cacheKey(uid, path, etag,
  x, y, fill)` is a fingerprint, so no filename leaks through the cache.
- Preview cache reads are content-derived: a preview of a v3-sealed file is
  a thumbnail of its plaintext — sealing previews under any server-held key
  would open a content side-channel for exactly the enrolled users ADR-0100
  protects (at-rest compromise with no active session must expose nothing).
- Preview serving runs in the requester's ctx: owner session, sharee wrap
  (ADR-0098), public-link anonymous ctx (master-wrapped owners only) — the
  same identity paths the file key's `Resolve` already honors, so per-file
  sealing inherits every access boundary for free.
- The background pregeneration job (ADR-0084) runs without any user session.
- The encrypt decorator's `Create` seals v3 whenever a resolver is
  configured; v1/v2 reads auto-detect, and the ADR-0074 keyring already
  reseals v2 under rotation.

## Decision

### 1. Ownerless keys fall back to master-key (v2) sealing

When a resolver is configured, the encrypt decorator's `Create` keeps the v3
path only for keys that name an owner (`ownerUID` non-empty — `uid/…`,
`uploads/<uid>/…`, `versions/<uid>/…`, `trash/<uid>/…`). Ownerless keys —
the `appdata_<instanceID>/` trees — seal **v2 under the current keyring
key**, exactly as multi-key master-mode writes do today. One predicate
(`OwnableStorageKey`, exported sibling of the resolver's `ownerUID`) decides;
no new tables, no resolver changes, reads unchanged (v2 auto-detects).

appdata content is instance-operational data; master-sealing it is the
ADR-0052 baseline, and a master-key compromise exposing it is unchanged from
master-mode deployments. The rotate-keys sweep covers these blobs like any
other v2 (verify its tree enumeration; extend if it skips appdata).

### 2. Previews of v3 sources self-seal under the source file's key (NCGOPV1)

The preview cache is the one confidentiality-sensitive appdata content. When
the SOURCE file is v3 (its filecache row carries `key_uuid`):

- The generator resolves the source's file key through a narrow seam
  (`KeyUUIDAt(ctx, uid, path)` on the DAV side + the existing `Resolve`) in
  the **requester's** ctx and seals the rendered blob as a single shot:

  ```
  NK_pv = HKDF-SHA256(FK, salt="", info="NCGOPV1" || keyUUID)
  blob  = "NCGOPV1"(8) || keyUUID(16) || nonce(12) ||
          AES-256-GCM(NK_pv, nonce, rendered, ad="NCGOPV1" || keyUUID || cacheKey)
  ```

  The cacheKey in the AD binds the blob to its exact variant (source etag,
  box, fill) — a variant-swap replay fails authentication. Previews are
  small; reads reject blobs over 32 MiB before decrypting.
- Sealed previews live under a NEW prefix `appdata_<id>/previews_enc/`,
  written and read via the RAW storage backend (the decorator never sees
  them). A separate prefix keeps rollback clean: an old binary simply never
  finds them (cache miss → regenerate → store under the old path), instead
  of sniffing an unknown magic and serving ciphertext as an image. GC and
  stats cover both prefixes.
- Read path: `previews_enc` hit → resolve the FK named by the header's
  keyUUID in the request ctx (owner session / sharee wrap / master-mode
  anonymous — identical to content reads) → open → serve. Any resolve or
  open failure maps like a content failure (locked enrolled file → 403, the
  ADR-0101 boundary — the source read would fail identically). A miss or
  legacy blob falls through to today's decorated `previews/` path, so all
  pre-existing cache entries keep working.
- Previews of non-v3 sources (plaintext, v1/v2) use today's decorated path
  unchanged — master-mode deployments are bit-identical.
- The **pregeneration job skips v3 sources it cannot resolve** (an enrolled
  user's FK in a principal-less ctx): debug-logged, not an error. Enrolled
  previews generate on first interactive request instead.

### 3. Everything else under appdata

Plugin storage and any future `appdata_` consumer ride rule 1. Per-plugin
keys are rejected: plugin data is server-operational, and the ADR-0042/0061
quota/sandbox boundaries are unchanged by master-sealing.

### Alternatives considered

- **Instance-key hierarchy** (a master-sealed "instance UK", per-object FKs
  wrapped under it, a wrap table): zero threat-model gain over v2 (both are
  master-sealed), but a new table, a resolver branch, and new rotation
  surface. The only confidentiality-sensitive appdata content is previews,
  and those are handled better by per-file sealing. Rejected.
- **Plaintext exemption for appdata**: loses at-rest sealing of appdata on
  every deployment, including master-mode ones that have it today. Rejected.
- **Seal previews under the decorator with v3 per-object keys** (resolver
  Allocate for preview keys): the decorator sits below the database and
  cannot map a preview key to its source file's recipient set; wrapping per
  recipient duplicates the share/revoke machinery for zero gain over sealing
  with the source FK directly. Rejected.
- **Do nothing** (loud failure stands): per-user mode would stay
  incompatible with previews and plugin storage. Rejected.

## Consequences

- Per-user mode stops breaking preview generation and plugin storage.
- Threat model, stated plainly: appdata content is master-sealed in every
  mode; previews of v3 files inherit the source file's confidentiality
  exactly (enrolled at-rest: nothing readable without a session; sharee and
  public-link boundaries match content). Preview cache keys were already
  pseudonymous; this adds content confidentiality to the cache.
- New narrow seams: `OwnableStorageKey` (encrypt package), a source-key
  lookup + FK resolution seam on the preview Generator (`KeyUUIDAt` via DAV
  + `Resolve`), and a raw-storage handle for the `previews_enc` prefix.
  Wired only in `app.go`; nil seams keep today's behavior (bit-identical
  master-mode).
- Rollback: v2 appdata and legacy previews read on any build with the
  keyring; `previews_enc/` is invisible to old binaries, which regenerate
  into the old path (the preview cache is self-healing by nature).
- The pregeneration job's coverage narrows for enrolled users (first
  interactive request pays the render cost). Documented in the runbook.

## Verification

- Decorator: with a resolver configured, `Create` on an appdata key writes a
  v2 header under the current key ID (bit-exact against a resolver-less
  multi-key ring); user keys still write v3; `OwnableStorageKey` truth table
  (user trees, uploads/versions/trash, appdata, malformed).
- Preview: a v3 source produces a `previews_enc` blob that opens only with
  the source FK (round-trip; wrong key/tamper/flipped cacheKey variant →
  ErrIntegrity; >32 MiB rejected pre-decrypt); sharee ctx and master-mode
  anonymous ctx open it; a locked enrolled ctx fails like the content read;
  non-v3 sources keep the decorated path (bit-identical); legacy `previews/`
  entries still serve; pregeneration skips unresolvable v3 sources without
  erroring; GC covers both prefixes.
- End to end: per-user mode on + previews on + a plugin writing storage —
  no loud failures anywhere; full gates (suite, race, lint, tidy) green.
