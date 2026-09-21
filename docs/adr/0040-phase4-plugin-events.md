# ADR-0040: Phase 4c2 Event Bus & Plugin Event Delivery

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Spec §6 defines `ncgo.event_publish` and manifest-driven subscriptions
(`events.subscribe` + the `ncgo_on_event` entry point); §10's walkthrough has
the files module emitting `files.uploaded` for plugin consumers. Phase 4a
registered `event_publish` as a stub returning `ErrUnsupported`. This
increment lands the real in-process bus and host-to-plugin delivery.

## Decision

1. **Synchronous fan-out bus (`internal/events`).** One `Bus` per process;
   `Publish` snapshots subscribers under `RLock` and invokes handlers after
   unlocking, so handlers may subscribe, unsubscribe, and publish
   re-entrantly without deadlock. No subscribers = no-op. A panicking
   handler is recovered, logged, and skipped. `Event.Source` is `"host"`
   for core-module emissions and `"plugin:<id>"` for plugin publishes.

2. **Self-skip.** The host dispatcher skips the plugin whose id matches
   `plugin:<id>` in `Event.Source`. A singleton plugin receiving its own
   publish would deadlock on the instance mutex (the outer call still holds
   it); skipping is also what subscribers expect.

3. **Delivery: alloc/write/call/free.** For each attached plugin with an
   `on_event` entry point and a matching `events.subscribe` glob, the host
   acquires an instance, allocates topic/payload buffers via the guest's
   `ncgo_alloc`, writes through module memory, calls `ncgo_on_event`, and
   frees via `ncgo_free`. Any trap (entry, alloc, or free) destroys the
   instance via `release(true)`; a non-zero i32 result is a `PluginError`.
   Delivery failures are logged warn with plugin.id + topic and never fail
   the `Publish` — one broken subscriber cannot starve the others.

4. **`event_publish` host function.** Reads topic + payload (payload copied
   out of guest memory), enforces `events.publish` globs with `core.*`
   reserved (`ErrPermissionDenied`), returns `ErrUnavailable` when the host
   has no bus, else publishes synchronously and returns `ErrCodeOK` — the
   publisher observes subscriber failures only as host log lines.

5. **First core emission: `files.uploaded`.** `DAV.Write` publishes after a
   successful commit (never failing the write) with a MessagePack payload
   `{"user", "path", "size", "created"}` per §10. Delete/rename events are
   out of scope for this increment.

6. **Wiring.** The app constructs one bus, shares it between the files DAV
   and `plugins.HostConfig.Bus`; `startOne` attaches each plugin after a
   successful start, `Plugin.Close` detaches, `Host.Close` unsubscribes the
   dispatcher.

## Alternatives Considered

### Async bus (queue + worker goroutines)
- Pros: publishers never block on slow plugins; natural backpressure point.
- Cons: ordering/loss semantics to define, delivery errors lose the
  publish-time context, and §11's < 500 µs per-subscriber budget is trivially
  met in-process. Deferred until a measured need exists.

### Direct subscription registry (no generic bus)
- Pros: one less abstraction.
- Cons: core modules would depend on the plugin runtime; a generic bus also
  serves future core-only consumers (activity, notifications).

## Consequences

- `Publish` is synchronous: a slow subscriber delays the publisher (bounded
  by the plugin's per-call timeout). Acceptable in-process; revisit with
  metrics.
- Event payloads crossing the ABI are opaque bytes; MessagePack is the
  convention (as with db rows), not enforced by the bus.
- `Event.Payload` is copied out of guest memory at publish time so handlers
  may retain it safely.
- Follow-ups: runtime enable/disable must refresh dispatcher attachments;
  delivery metrics (per-subscriber latency/failures, spec §12);
  persistent/async bus if cross-process or at-least-once delivery is needed;
  more core topics (`files.deleted`, share/calendar events).

## Verification

- Bus unit tests: fan-out order, unsubscribe, no-subscriber no-op, panic
  recovery, publish/subscribe from inside a handler (no deadlock).
- End-to-end through wasm guests on the real bus: host-origin delivery with
  glob matching (match, mismatch, empty payload), plugin-to-plugin publish
  with self-skip proven under the singleton model, `core.*` denial (-3),
  missing grant (-3), nil bus (-12), and a trapping/non-zero subscriber not
  affecting the publisher's OK or a healthy subscriber.
- files DAV test: one `files.uploaded` per write with the right
  user/path/size/created; overwrite reports `created=false`.
- `go test ./...` green, race clean, golangci-lint 0 issues.
