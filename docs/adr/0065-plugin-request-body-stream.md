# ADR-0065: Plugin route request body streaming (body_handle opt-in)

- **Status**: Accepted
- **Date**: 2026-09-23
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0041 (its "body_handle streaming is a follow-up" deviation
  is now closed as an opt-in)

## Context

ADR-0041 shipped plugin HTTP routes with the request body traveling inline
in the §7 request map as `body_bytes`, capped at 1 MiB with a 413 above —
deliberately, to keep dispatch simple — and recorded the specced
`body_handle` streaming protocol as a follow-up. Large uploads to plugin
routes (a files-app-style plugin receiving multi-MiB POSTs) are impossible
under the inline cap, so the follow-up is now due.

Two constraints shaped the design:

1. **Existing guests must not break.** A plugin compiled against the 4c3
   host reads `body_bytes` from the request map; silently switching the map
   to `body_handle` would hand every deployed plugin an empty body.
2. **The 413 semantic is worth keeping.** The inline cap is a cheap
   memory-abuse guard for the common small-payload route; removing it
   wholesale would force every route to stream whether it wants to or not.

Spec §9 explicitly allows new host functions within a major
("New host functions added within a major are OK; missing functions trap if
called"), so the ABI stays `ncgo-abi/1`.

## Decision

1. **Manifest opt-in, not a wholesale switch.** A new
   `[runtime] request_body_stream = true` key selects the specced §7 map:
   dispatch no longer pre-reads the body, wraps `req.Body` as a stream
   handle, and sends `{method, path, headers, query, body_handle}` with no
   `body_bytes` key. Plugins without the key get byte-identical 4c3
   behavior — inline `body_bytes`, 413 above 1 MiB. This satisfies both
   constraints: old guests are untouched, and the 413 guard remains the
   default posture while plugins that need large bodies opt in explicitly.
2. **Two new host functions**, registered for every module:
   `request_body_read(handle, buf_ptr, buf_max) -> i32` and
   `request_body_close(handle) -> i32`. Their semantics mirror
   `http_response_body_read`/`http_response_close` exactly: bytes read (>0),
   0 at EOF, -11 (`ErrTooLarge`) for a `buf_max` above the 1 MiB payload
   argument cap, -4 (`ErrNotFound`) for a missing handle, -2
   (`ErrInvalidArgument`) for a handle of any other type, -1 on read
   errors. No capability gates them — the body is the plugin's own inbound
   request, already authorized by the route capability at registration.
3. **Handle kind: the existing `handleStream` budget (64).** A request body
   *is* a stream, so it shares the spec §8 64-stream pool with storage
   streams instead of inventing a fourth handle kind and a new budget the
   spec does not define. Dispatch registers the handle on the acquired
   instance's table *before* invoking `ncgo_on_request` so the guest can
   read immediately.
4. **Lifecycle: instance cleanup is the backstop, never a double close.**
   The guest may close the handle early via `request_body_close`; a handle
   it never closes (e.g. it only read a prefix) is dropped by the existing
   handle-table cleanup when the instance is released — the same sweep that
   rolls back transactions and closes rows/responses for all three instance
   models. The wrapper's `Close` is a **no-op**: `req.Body` belongs to
   net/http, which closes it after the handler returns, so the handle only
   gates guest visibility. After `ncgo_on_request` returns the response
   streaming phase begins and the guest should not read the body any
   longer; the spec now states this.
5. **SDK:** `pluginsdk.RequestBodyRead(handle, buf) (int, int32)` /
   `RequestBodyClose(handle) int32` binding pair, and a `BodyHandle` field
   on `pluginsdk.HTTPRequest` so `DecodeHTTPRequest` exposes the handle
   (msgpack ignores the absent key for legacy maps). The host marshals
   `body_handle` as a Go `int` so small handle ids get msgpack's compact
   fixint encoding.

## Consequences

- Opt-in plugins can receive arbitrarily large route bodies: a 2 MiB POST
  that today gets a 413 is served, streamed through the handle in
  guest-chosen chunk sizes (the probe reads 64 KiB per call). The host
  never buffers the body; memory use per in-flight request stays O(chunk).
- Non-opt-in plugins are untouched by construction — same code path, same
  map, same 413.
- The stream budget is shared: a guest that opens 63 storage streams and
  then receives a streamed request still gets its body handle (64 total);
  the per-request handle is one slot out of a budget the plugin already
  manages.
- Per-instance handle ids grow monotonically for pooled/singleton plugins
  across requests; ids past 127 simply move msgpack to uint8/uint16
  encodings, which guests decoding a map integer handle natively.
- **Outbound streaming stays a follow-up.** `http_request` still takes
  `body_bytes` inline (≤ 1 MiB); >1 MiB guest-initiated uploads need a
  symmetric upload-handle ABI (guest-produced stream) which is a separate
  design surface and remains registered as future work.
