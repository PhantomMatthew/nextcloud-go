# ADR-0085: Phase 5l preview cache garbage collection (TTL sweep)

- **Status**: Accepted
- **Date**: 2026-09-24
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0053 (closes its cache-GC follow-up), ADR-0084 (same)

## Context

ADR-0053's preview cache is etag-keyed and hash-flat: under
`appdata_<instanceID>/previews/`, each entry's name is the sha256 of
`uid \n path \n etag \n "<x>x<y>"` plus a `.jpg`/`.png` extension. A file
rewrite, delete, or move orphans its entries, and the cache grows by every
distinct (etag × box) combination ever requested — ADR-0053 accepted this
explicitly ("until a GC follow-up lands"), and ADR-0084's opt-in
pregeneration worsened it by writing hot-size entries for files nobody may
ever view.

Exact orphan detection is impossible without a layout change: the keys are
one-way content-derived hashes, so an entry cannot be reverse-mapped to its
(file, box) to check whether that file still exists with that etag. An
orphan is only ever *reachable* again when uid, path, etag, and box all
match — i.e. identical content — which makes orphans pure storage waste,
never a correctness issue.

## Decision

- **modtime-TTL sweep.** A new periodic job `preview.gc`
  (`preview.NewGCJob`, wired in `app.go` when `previews.enabled` is set)
  lists the flat cache prefix and deletes every entry whose modtime is
  older than `previews.cache_max_age` (default 720h / 30 days; validation
  rejects values below 1h, 0 selects the default). TTL deletion is safe
  precisely because of the one-way keys: the worst case is deleting a
  still-reachable hot entry, which costs one regeneration on the next
  request — it can never serve wrong bytes, since a wrong-bytes entry would
  require an etag that no longer matches. Subdirectories are skipped: the
  cache is flat by design, so anything unexpected is left alone.
- **24h in-memory throttle against the 5s re-enqueue.** The periodic
  runner re-enqueues the job after every poll interval (default 5s) — far
  too hot for a full List+Delete pass. The job records its last completed
  pass (mutex-guarded, in-memory) and no-ops until 24h have elapsed. A
  process restart costs one immediate sweep, which is harmless.
- **Per-entry-failure tolerance.** The runner retries a failed `Run`
  forever, so one stubborn entry must not poison the queue: individual
  Delete failures are Warn-logged and skipped, and the sweep still counts
  as completed. Only a List infrastructure failure returns an error — with
  the throttle timestamp deliberately left unset so the runner's retry
  sweeps immediately. A missing cache directory (`storage.ErrNotFound`)
  counts as a completed empty pass.
- **Gating on previews.enabled.** The job exists only when the generator
  does (the cache prefix is `a.previewGen.CachePrefix`), and registration
  precedes `jr.Start` because the runner seeds periodic jobs only for
  already-registered names (ADR-0014).

## Alternatives Considered

### Layout change: embed uid/path/box in entry names
- Pros: exact orphan detection — every entry reverse-maps to its file, and
  a sweep can check filecache for (path, etag) liveness.
- Cons: invalidates every existing cache entry on upgrade, complicates the
  hash-flat scheme ADR-0053 chose deliberately (names that reveal nothing
  about paths), and still cannot enumerate the box dimension — clients may
  request any x/y up to `max_dimension`, so "all live entries" remains
  uncomputable without a box registry (below). Rejected.

### Touch-on-read LRU
- Pros: hot entries never age out; the TTL becomes a true
  least-recently-used bound.
- Cons: puts a metadata write on the hot read path that ADR-0053 kept
  read-only (cache hits today cost one Stat + one Open). A once-a-month
  regeneration for a continuously hot entry is cheaper than a write per
  read. Rejected.

### Box registry for exact recompute
- Pros: exact: enumerate live (uid, path, etag) from the filecache, cross
  a registry of requested boxes, recompute every reachable key, delete the
  rest.
- Cons: a new table plus a write on every preview serve to record boxes —
  significant machinery to be exact about a problem the TTL solves to
  within bounded storage waste. Rejected.

## Consequences

- New config surface: `previews.cache_max_age` (default **720h**).
- The cache is now bounded by 30 days of distinct (etag × box) instead of
  growing forever; hot entries older than the TTL regenerate exactly once.
- Single-node assumption: the 24h throttle lives in process memory, which
  is sufficient because the SQLRunner is single-node (ADR-0014); a future
  multi-node runner would need a shared home for it.
- Follow-ups: none required.

## Verification

- `internal/preview/gc_test.go` (real localfs on `t.TempDir()`): an entry
  aged 31 days via `os.Chtimes` is swept while a fresh entry and an aged
  subdirectory survive; a missing prefix is a nil completed pass that still
  starts the throttle; after a sweep an immediate rerun deletes nothing and
  a 25h clock advance re-arms it; a stub storage proves one failing Delete
  never fails the sweep while the other entry is still deleted, and a
  non-`ErrNotFound` List error propagates with the throttle left unset so
  the retry is immediate; nil-cache and empty-prefix guards plus `Name()`.
- `internal/config` tests pin the 720h default and the validation rule
  (30m rejected, 0 and >= 1h accepted).
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` clean (no new
  dependencies).
