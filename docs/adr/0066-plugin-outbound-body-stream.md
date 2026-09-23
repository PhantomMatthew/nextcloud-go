# ADR-0066: Plugin outbound request body streaming (http_request body_handle)

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0043 (its ">1 MiB uploads need streaming requests"
  follow-up is now closed), ADR-0065 (its outbound-streaming follow-up is
  now closed)

## Context

ADR-0043 shipped outbound HTTP with the request body traveling inline in the
`http_request` map as `body_bytes`, capped at 1 MiB (`maxPayloadArg`), and
registered streaming uploads as a follow-up; ADR-0059 (rate/response caps)
and ADR-0065 (inbound `body_handle`) each repeated it. A plugin producing a
multi-MiB upload (e.g. forwarding a received file to an object-storage
endpoint) cannot send it today.

The design surface is how a guest hands a large body to the host. Two
options were considered:

1. **Live streaming** — the guest produces chunks while `http_request`'s
   `client.Do` reads them (an `io.Pipe` between guest writes and the
   request body).
2. **Spool** — the guest stages the body in host-side chunks into a temp
   file, seals it, and `http_request` streams the file to the server.

Live streaming fails on three counts:

- **Call lifecycle.** The guest only runs while the host calls it, but
  `client.Do` consumes the request body asynchronously — after the
  `http_request` host call has been entered and the guest is suspended
  waiting for its return. Pumping an `io.Pipe` would need a per-request
  goroutine plus re-entrant guest calls *inside* another host call, which
  the instance model (one call per instance at a time, traps destroy it)
  does not support.
- **Timeout semantics.** The per-call wall-clock timeout
  (`runtime.cpu_timeout_ms`, host-capped at 30s) governs one host call. A
  live upload spans the whole `client.Do`, so either the guest's write loop
  rides on `http_request`'s deadline (killing slow-but-legal uploads at
  30s) or the deadline model gains a special case with muddy ownership of
  "which call is this work billed to".
- **Failure propagation.** With a pipe, an upstream that resets the
  connection mid-upload cannot be reported back to the guest — it already
  handed the bytes off and holds no pending call to learn the outcome from.
  The spool makes staging and sending two phases with one clear result
  channel: staging errors come from the write calls, send errors from
  `http_request`.

Spec §9 explicitly allows new host functions within a major ("New host
functions added within a major are OK; missing functions trap if called"),
so the ABI stays `ncgo-abi/1`.

## Decision

1. **Spool, not live streaming.** Three new host functions stage the body in
   a temp file (`os.CreateTemp`, the ADR-0042 storage-spool mechanism):
   - `http_request_body_create() -> i64` — high32 err / low32 handle; gated
     on the `http.outbound` capability exactly like `http_request` (a body
     the plugin may never send is dead weight on disk). No new capability
     key: the per-target allowlist still gates the request itself.
   - `http_request_body_write(handle, buf_ptr, buf_len) -> i32` — appends
     one chunk; `buf_len` above the 1 MiB payload cap fails with -11, and
     the write that would push the running total past the spool cap fails
     with -11.
   - `http_request_body_close(handle) -> i32` — seals the spool; a sealed
     handle rejects further writes and repeated closes with -2 but stays in
     the handle table for `http_request` to consume.
2. **The spool cap reuses `HostConfig.MaxSpoolBytes`** (default 1 GiB) —
   the same knob as the storage write spools. Guest-staged bytes on local
   disk are one resource class; one operator-visible limit for all plugin
   spools, no new configuration surface.
3. **`http_request` map gains `body_handle`** (mutually exclusive with
   `body_bytes`; both present → -2). The handle must name a *sealed* spool:
   an unsealed handle is -2 and stays usable, a missing id -4, an id of
   another handle type -2. Consumption is destructive ("consume destroys"):
   the handle leaves the table, the request goes out with a known
   `ContentLength` (and a `GetBody` that reopens the spool so 307/308
   redirects can replay it), and the spool file is deleted when the call
   ends — success or failure. A spool never consumed is deleted by the
   instance handle-table cleanup, like every leftover handle.
4. **Handle kind: the existing `handleStream` budget (64)** — the ADR-0065
   reasoning again: a body is a stream, and the §8 budgets define no fourth
   kind.
5. **The 4l/4n defenses are orthogonal and unchanged.** Target allowlist,
   the dial-time private-IP egress guard (ADR-0057), the per-plugin rate
   limit and redirect re-validation (ADR-0059), and the response byte cap
   all apply to the streamed path exactly as to the inline path — only the
   body source differs. Allowlist and rate-limit denials are checked
   *before* consumption, so a throttled or disallowed request leaves the
   sealed spool intact for a guest retry.
6. **SDK:** `pluginsdk.HTTPBodyCreate() (int32, int32)`,
   `HTTPBodyWrite(handle, buf) (int, int32)`, `HTTPBodyClose(handle) int32`,
   and a `BodyHandle int32` field on `HTTPOutboundRequest`
   (`msgpack:"body_handle,omitempty"` — requests that do not stream keep
   the 4c5 map shape byte-identical, and pre-4t hosts ignore the unknown
   key should one ever reach them).

## Consequences

- Plugins can upload bodies of arbitrary size up to the spool cap (default
  1 GiB) with guest memory use O(chunk): the host never holds the body in
  RAM — chunks land on disk and the client streams the file.
- Sending starts only after the last write plus seal: upload latency gains
  the staging time, and there is no backpressure coupling between the
  server and the guest. That is the price of the simple two-phase failure
  model; both phases are individually bounded (per-call timeout for writes,
  `timeout_ms` ≤ 30s for the request).
- The inline `body_bytes` path is byte-identical to 4c5; only guests that
  opt into the new functions produce a `body_handle` map.
- A guest compiled against the 4t SDK running on a pre-4t host fails to
  instantiate (the imports are absent) — the standard §9 posture for new
  functions, same as 4s.
- `GetBody` reopens the spool per redirect hop, so replayed bodies are
  re-read from disk; redirect targets remain re-validated against the
  plugin allowlist per hop, unchanged.
