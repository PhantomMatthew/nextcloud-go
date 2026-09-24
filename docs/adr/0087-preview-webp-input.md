# ADR-0087: Phase 5n WebP preview input

- **Status**: Accepted
- **Date**: 2026-09-24
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0053 (closes the input half of its WebP follow-up)

## Context

ADR-0053 restricted preview sources to JPEG, PNG, and GIF and deferred
WebP "once `x/image/webp` decode coverage is sufficient". That coverage
has arrived: the pinned `golang.org/x/image v0.46.0` decodes both lossy
(VP8) and lossless (VP8L, including alpha) still WebP. WebP is a common
upload format — phone cameras, screenshot tools, and web saves all emit
it — so those files currently 404 from the preview endpoint and fall back
to original downloads in clients.

All ADR-0053 constraints continue to bind: content sniffing is the only
type authority, the decompression-bomb guard runs before pixel decoding,
and a failed parse must never serve source bytes or distinguish "missing"
from "unreadable".

## Decision

1. **Registration only, no new dependency.** `_ "golang.org/x/image/webp"`
   registers the decoder with `image.Decode`; the package already ships in
   the approved `golang.org/x/image` module (ADR-0053's exception), so the
   dependency graph is unchanged.

2. **Sniff whitelist adds `image/webp`** in both gates: `render` (serve
   path) and the `Pregenerate` head-sniff, via the new `mimeWebp` constant
   (the GIF literal is hoisted to `mimeGIF` alongside). `http.DetectContentType`
   identifies WebP by its RIFF/WEBP signature regardless of VP8 flavour, so
   sniffing stays definitive.

3. **Output re-encodes as PNG.** No WebP *encoder* exists in the stdlib or
   `x/image`, and the `format == "jpeg"` branch in `render` is untouched, so
   WebP sources — like GIF — emit PNG, preserving VP8L alpha. Animated WebP
   (VP8X animation chunks) fails `image.Decode` ("webp: invalid format")
   and collapses to the uniform 404, exactly like a corrupt or missing
   file; clients already tolerate missing previews.

4. **Bomb guard unchanged.** `DecodeConfig` parses the WebP header before
   any pixel decode, so the existing 1..8192 bounds apply verbatim; the
   256 MiB source cap likewise already covers the new format.

## Alternatives Considered

### WebP output (content-negotiated)
- Pros: smaller responses for webp-capable clients.
- Cons: requires an encoder dependency the project does not have and will
  not add for this; PNG/JPEG output is universally decodable. Deferred.

### Third-party decoder (chai2010/webp, gen2brain/webp)
- Pros: animated WebP, encoding.
- Cons: cgo or sizeable pure-Go ports for a thumbnail path; the
  semi-standard `x/image` decoder covers the still-image hot path.
  Rejected.

## Consequences

- Uploaded WebP images get previews (and pregeneration, when ADR-0084 is
  enabled) instead of 404s.
- Animated WebP remains 404 — indistinguishable from other unpreviewable
  content, preserving the no-oracle rule.
- WebP **output** stays unimplemented; the follow-up line in ADR-0053 is
  narrowed to output only.

### Follow-ups (explicit, not in v1)

- **WebP output** behind `Accept` negotiation, if an encoder ever enters
  the approved dependency set.

## Verification

- Real fixtures under `internal/preview/testdata/` (generated with PIL,
  checked in): lossy 100×60, lossless-with-alpha 80×50, animated 40×40.
- `internal/preview/webp_test.go`: lossy decodes and fits to 64×38 with
  `Content-Type: image/png` and exactly one `.png` cache entry; lossless
  alpha survives the PNG re-encode (opaque-red vs fully-transparent
  quadrants asserted on the decoded response); animated returns 404;
  `Pregenerate` warms WebP uploads (head-sniff gate lets them through).
- Full preview package suite green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` clean (no new
  dependencies).
