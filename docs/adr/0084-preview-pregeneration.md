# ADR-0084: Phase 5k preview pre-generation on upload events

- **Status**: Accepted
- **Date**: 2026-09-24
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0053 (closes its preview pre-generation follow-up)

## Context

ADR-0053 generates previews lazily on first request and deferred
"generating previews eagerly on upload (background job)" as a follow-up:
its stated cons were CPU spent on files nobody ever views and the need for
a jobs hook into `files.uploaded` plus invalidation bookkeeping. The hook
now exists — `DAV.Write` publishes `files.uploaded` on the event bus with
a msgpack payload (`user`, `path`, `size`, `created`) — and the
etag-keyed cache layout from ADR-0053 already makes invalidation implicit,
so the bookkeeping concern is gone. What remains is the wasted-CPU con,
which is why the feature is opt-in. First loads of the files list (32px)
and gallery (256px) still pay an on-demand decode per image; operators who
prefer first-hit cache hits over idle CPU can now move that work to upload
time.

## Decision

- **Event-driven enqueue, payload forwarded verbatim.** When
  `previews.pregenerate_enabled` is set (and previews are enabled),
  `app.go` registers a `preview.pregenerate` job with the jobs runner and
  subscribes to the bus: every `files.uploaded` event enqueues one row
  carrying the event's msgpack payload unchanged. The job decodes only
  `user` and `path`; an undecodable payload is Warn-logged and dropped.
  The topic string is now the exported `files.EventFilesUploaded` constant
  so producer and consumer cannot drift.
- **Head-sniff before the full read.** `Generator.Pregenerate` opens the
  source through `DAV.Read` (same access gate as serve), reads at most 512
  bytes, and runs `http.DetectContentType` first: anything that is not
  JPEG/PNG/GIF is skipped without reading the rest. Unlike the serve path
  — which a client explicitly asked for — the background path must not
  `ReadAll` up to 256 MiB of every uploaded video. Sniff-passing files are
  then read under the remaining `maxSourceBytes+1-n` limit and rendered
  through the same `render` (sniff → DecodeConfig bounds → decode → fit-box
  scale → JPEG/PNG encode) the endpoint uses, extracted unchanged from
  `generate` so the HTTP path stays byte-identical.
- **Hot sizes in config, clamped and deduped.**
  `previews.pregenerate_sizes` (default `[32, 256]` — the files list and
  gallery boxes) names square box edges; validation bounds each entry to
  1..4096 and rejects `pregenerate_enabled` with `previews.enabled=false`
  (same dead-key rule as `metrics_listen`, ADR-0076) or with an empty size
  list. `NewPregenerateJob` clamps each size to the generator's `MaxDim`,
  drops non-positive entries, and dedupes preserving order, so a 4096
  configured against `max_dimension` 2048 renders one 2048 box, not two
  overlapping ones.
- **Idempotent under at-least-once delivery.** The jobs runner delivers at
  least once, so re-runs must be free: per box, the etag-keyed cache is
  checked first and a hit skips the render; misses run under the same
  singleflight group as the serve path (with the same in-flight cache
  re-check), so a concurrent client request and a pregeneration run cannot
  decode the same preview twice.
- **Per-file conditions are nil; infrastructure retries.** The runner
  reschedules a failed `Run` forever, so the job returns nil for every
  per-file outcome — bad payload, missing/forbidden/deleted file, root or
  unnormalizable path, empty or unreadable body, non-image, oversized,
  corrupt — and returns an error only for infrastructure failures (the
  `DAV.Read` itself failing, or a non-`errNotPreviewable` render error such
  as an encode failure), which a retry can genuinely fix. Cache-write
  failures are Warn-logged and never fail the run, matching the serve
  path's store semantics.

## Alternatives Considered

### Periodic filecache sweep
- Pros: no event wiring; also covers files uploaded while the feature was
  off.
- Cons: enumerating the whole file tree on a schedule to find "new" images
  needs MIME/extension trust (rejected below) or a head read per file per
  sweep, and adds scheduling machinery for what a single event subscription
  covers precisely. Rejected.

### Eager generation inline in `DAV.Write`
- Pros: no jobs row at all; previews exist before the write returns.
- Cons: blocks every upload on up to N decode+scale+encode cycles, turning
  a bulk import into a CPU stall and coupling write latency to image
  content. The jobs runner already provides the asynchronous, retryable
  execution this work wants. Rejected.

### Filtering enqueues by filecache MIME / extension
- Pros: skips the event for obvious non-images.
- Cons: ADR-0053 already rejected uploader-supplied MIME as an authority;
  the filecache MIME is exactly that. The 512-byte head-sniff is cheap and
  definitive, and keeping the job MIME-agnostic preserves the endpoint's
  never-trust-metadata stance. Rejected.

## Consequences

- New config surface: `previews.pregenerate_enabled` (default **false** —
  opt-in, per ADR-0053's wasted-CPU concern) and `previews.pregenerate_sizes`
  (default `[32, 256]`).
- When enabled, every successful upload inserts one jobs row; image uploads
  cost up to len(sizes) decode+encode cycles at upload time instead of at
  first view. Non-image uploads cost one 512-byte read.
- Cache growth semantics are unchanged from ADR-0053: etag-keyed entries,
  implicit invalidation on rewrite, stale entries orphaned until GC.
- The HTTP preview path is byte-identical; only the render pipeline moved
  into a shared helper.

### Follow-ups (explicit, not in this phase)

- ~~**Cache GC** of orphaned etag-keyed entries remains open (carried from
  ADR-0053): pregeneration grows the cache by hot-size entries for files
  that may never be viewed, which strengthens the case.~~ (**resolved by ADR-0085**)

## Verification

- `internal/preview/pregenerate_test.go` (real localfs storage + sqlite
  files DAV, uploads via `DAV.Write`, payloads captured from the real bus):
  PNG and JPEG uploads pre-generate exactly the configured boxes with
  correct fit dims and source-derived extensions (jpeg → .jpg, png → .png);
  text, encrypted garbage, corrupt images, and never-uploaded paths return
  nil with zero cache entries; a second Run of the same payload creates
  nothing new (counting storage); sizes `[32, 4096, 32]` against MaxDim
  2048 produce exactly the 32 and 2048 boxes; garbage, wrong-shape, and
  missing-field payloads return nil.
- `internal/config` tests pin the new defaults and the validation rules
  (size 0 / 4097, enabled without previews.enabled, enabled with empty
  sizes).
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` clean (msgpack was
  already a direct dependency; nothing new).
