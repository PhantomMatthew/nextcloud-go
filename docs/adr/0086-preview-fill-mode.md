# ADR-0086: Phase 5m preview fill mode (aspect-fill centre crop)

- **Status**: Accepted
- **Date**: 2026-09-24
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0053 (closes its fill/crop follow-up for `mode=fill`)

## Context

ADR-0053 v1 ignores the `mode` query parameter: every preview is an
aspect-preserving **fit** inside the requested box. Nextcloud clients
actually request `mode=fill` for square thumbnails (the files app grid and
gallery views), where the preview must *cover* the box exactly — the client
renders it into a fixed square cell. Serving those clients a fit image
leaves letterboxing to the browser and produces visibly inconsistent grids.

Constraints carried over from ADR-0053:

- Never upscale: a source smaller than the box keeps its resolution.
- The cache key must distinguish rendering variants of the same
  (uid, path, etag, box) — a fill render must never be served for a fit
  request or vice versa.
- Unknown or unsupported `mode` values must keep working as before (client
  tolerance): they fall back to fit, they never become a 400.

## Decision

1. **`mode=fill` is the only honoured value.** `q.Get("mode") == "fill"`
   selects the fill path; every other value — including `crop` — and an
   absent parameter keep the v1 fit behaviour. No new 400 cases: parameter
   validation is unchanged.

2. **Fill algorithm: cover-scale, then centre crop, never upscale.**
   `factor = min(max(x/w, y/h), 1.0)` scales so the source covers the box
   (capped at 1.0 like fit); the result is centre-cropped to
   `min(dw, x) × min(dh, y)`. When the source is larger than the box the
   output is exactly `x × y`; a smaller source keeps its size on the
   short axis (the never-upscale rule wins over exact box coverage). The
   crop copies pixels through `draw.Draw` rather than `SubImage` so
   non-zero source bounds stay correct. Scaling keeps using
   `draw.ApproxBiLinear`.

3. **Cache key gains a fill marker, fit keys are bit-identical.** The v1
   key hashes `uid "\n" path "\n" etag "\n" "<x>x<y>"`; fill appends
   `"\nfill"` to that string before hashing. Existing fit cache entries
   therefore remain valid with zero invalidation churn, fit and fill
   variants coexist under the prefix, and the response `ETag` (the cache
   key) is mode-qualified for free. Singleflight separation falls out of
   the key change with no extra work.

4. **Plumbing, not new surface.** `fill bool` threads through
   `serve` → `generate` → `render` → `scale|scaleFill`. No new endpoint,
   config key, or response header.

5. **Pregeneration warms fit boxes only** (ADR-0084 unchanged): doubling
   the upload-time CPU for fill variants nobody may request contradicts
   the opt-in economy of that feature. Clients requesting fill simply miss
   once and populate the cache, exactly as before ADR-0084.

## Alternatives Considered

### Full NC `crop` mode (offset-based cropping)
- Pros: complete client compatibility.
- Cons: NC's crop mode takes offset semantics the follow-up never
  specified, and no known client of the file-path preview endpoint uses
  it. Falling back to fit keeps those requests correct, just
  letterboxed. Deferred until a real client demands it.

### Embedding mode as a visible filename component
(e.g. `<hash>.fill.jpg`)
- Pros: cache listings are self-describing.
- Cons: changes the on-disk layout for zero functional gain; the key is
  already opaque and content-derived by design (ADR-0053 §5).

### Upscaling small sources to exact box coverage in fill mode
- Pros: output always exactly `x × y`, simpler client assumptions.
- Cons: violates the never-upscale rule from ADR-0053 and produces blurry
  previews; NC itself does not upscale previews. Rejected.

## Consequences

- Grid/gallery clients requesting `mode=fill` get exact-box, centre-cropped
  thumbnails instead of letterboxed fits.
- Cache at most doubles per (file, box): one fit and one fill variant.
  Growth stays bounded by the ADR-0085 TTL sweep.
- Offset `crop` remains unsupported and falls back to fit.

### Follow-ups (explicit, not in v1)

- **Offset `crop` mode** if a real client requires it.
- **Fill variants in pregeneration** if grid-view traffic justifies the
  upload-time CPU.

## Verification

- `internal/preview/fill_test.go`: centre-crop content assertions on a
  quadrant-coloured 800×600 source (output exactly 256×256, edges land in
  the correct quadrants); never-upscale (100×50 in a 256 box stays
  100×50); partial crop (800×600 into a 1024×256 box yields 800×256);
  `mode=crop`/`mode=bogus`/absent all fall back to fit and share one ETag;
  fit and fill cache entries coexist and fill hits the cache with zero
  source bytes re-read; JPEG fill stays JPEG; `cacheKey` fit format pinned
  byte-for-byte against the v1 hash input, fill differs by the `"\nfill"`
  suffix.
- All pre-existing preview and pregeneration tests pass unchanged.
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` clean (no new
  dependencies).
