# ADR-0081: Phase 5i precompressed static assets (.br/.gz sidecar negotiation)

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0054 (resolves its precompressed-assets follow-up)

## Context

ADR-0054 listed precompressed assets as a follow-up: compiled Nextcloud
frontend bundles are large, and operators who pre-compress their web root
(e.g. `brotli -k` / `gzip -k` sidecars, as Nextcloud's own packaging and
many deployment playbooks produce) should have those sidecars served
directly instead of the identity bytes. ncgo compresses nothing at
runtime — serving a pre-made sidecar is free throughput with zero CPU.

## Decision

`StaticUI.serveFile` (`internal/web/static.go`) negotiates sidecars:

- **Selection**: parse `Accept-Encoding`; brotli wins over gzip when the
  client accepts both (fixed server preference, matching every CDN
  default). A sidecar `<file>.br` / `<file>.gz` is used only when it
  exists as a regular file **contained in the root** — the same
  EvalSymlinks+containment check as the asset itself; an escaping
  sidecar symlink falls through to identity, never 404 and never an
  escape (the asset itself is servable).
- **q-values**: an explicit token with `q=0` excludes the encoding; a
  missing or unparseable q counts as q=1. Wildcards (`*`) do NOT select
  a sidecar — the nginx `gzip_static` stance; only explicit `br`/`gzip`
  tokens negotiate.
- **Response shape**: sidecar bytes are served with
  `Content-Encoding: br|gzip` and the **source asset's** name, content
  type, cache policy, and modtime — a 304/If-Modified-Since decision
  keys to the content version (the source), not to a sidecar file that
  may have been regenerated later. Cache policy therefore still keys to
  the source basename: immutable hashed bundles stay immutable in both
  representations.
- **`Vary: Accept-Encoding` on every asset response**, sidecar or not:
  the representation depends on the header whenever a sidecar exists,
  and a shared cache cannot see the filesystem to know that.
- **The injected SPA shell is excluded**: `serveShell` rewrites the
  bytes per session (ADR-0064/0069), so no precompressed representation
  of the shell can exist — an `index.html.br/.gz` sidecar is never
  consulted for it. Directory `index.html` documents that are not the
  shell (no injector armed) do negotiate sidecars, which is correct.

## Alternatives Considered

### Runtime compression (gzip middleware)
- Pros: works for operators who do not pre-compress.
- Cons: CPU per response (or a compression cache with its own
  invalidation problem), and pointless for the immutable bundles that
  dominate bytes — those are exactly the files operators pre-compress
  once at deploy time. Sidecars are the zero-CPU answer; a runtime
  layer can still be added later for API responses if profiling asks.

### Honoring wildcard and q-weighted ordering
- Cons: marginal real-world value (browsers send explicit token lists),
  and q-sorting buys nothing over the fixed br>gzip preference when both
  parse — the server preference is the operator's anyway (they chose
  which sidecars to generate). Explicit-token-only keeps the parser
  ten lines and predictable.

## Consequences

- Operators who pre-compress see bundle transfer sizes drop to the
  compressed size with zero server CPU; operators who do not see
  byte-identical behavior plus one `Vary: Accept-Encoding` header.
- Asset responses always carry `Vary: Accept-Encoding` — a small,
  correct cache-key widening shared caches already perform for origins
  that negotiate encoding.
- No config surface: sidecars are used when present, ignored when not,
  matching the zero-config stance of the static server.

## Verification

- `internal/web/static_precompressed_test.go`: brotli preferred with
  both sidecars present; gzip fallback (gzip-only acceptance, and
  br-accepted-but-no-.br-sidecar); identity for no header / q=0 /
  wildcard-only; `Vary` on encoded and identity; 304 keyed to the source
  modtime on both representations and source `Last-Modified` advertised
  on the encoded response; the injected shell never sidecar-served even
  with `index.html.gz` present; escaping sidecar symlink falls back to
  identity (200, source bytes); HEAD headers with no body; the
  `acceptsEncoding` parser table (case, whitespace, q variants,
  non-token prefixes).
- Full `internal/web` suite green (existing cache/304/traversal tests
  unaffected); `golangci-lint run ./...` 0 issues; no new dependencies.
