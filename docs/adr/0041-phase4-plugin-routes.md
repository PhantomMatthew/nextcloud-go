# ADR-0041: Phase 4c3 Plugin HTTP Routes & OCS Endpoint Dispatch

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Spec §6.3 defines `ncgo.route_register`/`ncgo.ocs_register` (callable only
during `on_install`) and §7 defines the `ncgo_on_request` entry point plus
the `ncgo_response_*` export family. Phase 4a registered both host functions
as stubs returning `ErrUnsupported` after the capability check. This
increment lands real registration (persisted route records) and HTTP
dispatch from the app router into guest modules.

## Decision

1. **Hook-only registration, persisted.** `route_register`/`ocs_register`
   run only inside lifecycle hooks (`inHook`, like DDL): outside a hook the
   host returns `ErrPermissionDenied`. Checks run in order: string reads,
   hook context, `routes.register`/`ocs.register` prefix grant, path under
   the plugin's own `/apps/<plugin_id>/` namespace (`ErrInvalidArgument`),
   uppercase method allowlist (GET/HEAD/POST/PUT/DELETE/PATCH/OPTIONS), and
   handler name (non-empty, ≤128 bytes, `[A-Za-z_][A-Za-z0-9_.]*`). Records
   persist to a new `plugin_routes` table (PK plugin_id+kind+method+path,
   upsert semantics) so routes survive restarts and are mounted at boot
   without running the plugin. Uninstall deletes a plugin's route rows
   alongside its registry row.

2. **Boot-time-only mounting.** `MountRoutes` runs once at the end of
   `mountRoutes`, after `StartEnabled`: for each started plugin it reads its
   persisted records and registers exact-path handlers on the app router
   (plain routes at the recorded path, OCS endpoints under both
   `/ocs/v1.php` and `/ocs/v2.php`). Runtime install/enable/disable route
   refresh is future work — a restart (or reinstall) is required to pick up
   new routes (closed by ADR-0062's runtime reconciler). A per-plugin
   registry read failure is logged and skipped
   (StartEnabled philosophy); a nil router/registry is a hard error. Two
   records colliding on method+path resolve last-wins (the router's exact
   map), same as any double registration.

3. **Dispatch: alloc/write/call/free on one instance.** The handler packs
   the request as MessagePack, acquires an instance, copies the payload in
   via `ncgo_alloc`, calls the manifest's `on_request` entry point, frees
   the payload, and unpacks the i64 (high32 error / low32 response handle)
   per the db.* convention. Status, headers, and the streamed body are then
   read through `ncgo_response_*` on the **same** acquired instance; any
   trap marks the instance broken so `release(true)` destroys it, and
   `ncgo_response_close` is always called best-effort. Missing exports,
   traps, non-zero high32, invalid status (<100/>599) → 502 with a warn log.
   The dispatch CallContext carries the authenticated UID, request id,
   Accept-Language locale, and a `callTimeout` deadline.

4. **`body_bytes` inline (spec deviation).** The request map is
   `{method, path, query, headers, body_bytes}`: the body travels inline,
   capped at 1 MiB (`maxPayloadArg`), instead of §7's `body_handle`
   streaming. Larger bodies are rejected with 413 before the plugin is
   invoked. This keeps v1 dispatch a single round-trip; a handle-based body
   can be added later without breaking `body_bytes` consumers.

5. **Headers as "Name: Value" lines.** `ncgo_response_header_at` writes one
   `"Name: Value"` line into host-allocated guest memory (out_max
   maxStringArg). The host splits on the first `": "`, skips malformed lines
   with a warn log, and ignores the plugin's Content-Length (the host sets
   it). Response bodies stream in 32 KiB chunks; a failure mid-stream is
   logged and the connection closes (status is already on the wire).

6. **OCS envelope wrapping.** For kind `ocs` the guest body must be a JSON
   document (invalid JSON → 502); it becomes the envelope's `data` element
   with objects converted to `ocs.OrderedMap` (sorted keys — raw maps are
   not renderable). On a 2xx plugin status the meta code is 100 (v1) / 200
   (v2) and the HTTP status is `ocs.Map` of that (200); on failure the meta
   code is 996 (`RespondServerError`, the repo's generic-error convention)
   with message "plugin request failed" and the HTTP status is the plugin's
   own status code. Format is negotiated per request (`?format=` / Accept).
   Plugin response headers are not propagated on OCS endpoints — the
   envelope owns the content type.

7. **Auth middleware choices.** Plain routes are wrapped with
   `webdav.Auth(authCfg)` (same chain as DAV routes: app password, bearer,
   session); OCS endpoints use `ocs.Auth(ocs.V1/V2, authCfg)` exactly like
   the built-in OCS handlers, so unauthenticated requests get the
   version-correct OCS 401 envelope.

## Alternatives Considered

### Handler-name-per-route dispatch (invoke `handler_name` directly)
- Pros: multiple distinct handlers per plugin without a guest-side router.
- Cons: doubles the export surface the host must resolve and verify;
  §7 defines a single `ncgo_on_request` entry and the response handle
  protocol is entry-agnostic. The manifest `on_request` entry point is the
  dispatch target; `handler_name` is persisted for observability and a
  future multi-handler increment.

### body_handle streaming per spec §7
- Pros: no 1 MiB cap; symmetric with outbound http_response_body_read.
- Cons: a second handle protocol (guest reads its own request body via host
  functions) with the same trap/EOF edge cases as response streaming —
  deferred until a plugin needs > 1 MiB requests.

## Consequences

- Routes registered by an install hook only take effect after a restart
  (mount-time is boot-time). Documented for plugin authors; runtime refresh
  is a follow-up.
- Request bodies are memory-resident on both sides of the ABI (≤1 MiB).
- The OCS failure path always reports meta code 996 regardless of the
  plugin's HTTP status; finer OCS codes would need a mapping convention
  plugins cannot express yet.
- Follow-ups: runtime install/enable/disable route refresh, per-route
  `handler_name` dispatch, `body_handle` for large uploads, dispatch
  metrics (per-route latency/failures, spec §12).

## Verification

- Host-function matrix through wasm guests: hook-only (-3 outside hooks),
  missing grant (-3), wrong namespace (-2), bad method (-2), empty/invalid/
  oversized handler name (-2), nil registry (-12), persisted row, upsert
  re-registration, OCS kind persisted, route rows deleted on uninstall.
- Dispatch end-to-end through httptest: status/headers/body streamed, guest
  observes method+path+query+body, plugin Content-Length ignored, malformed
  header line skipped, oversized body → 413 without invoking the plugin,
  high32 error / trap / missing response exports → 502, OCS v1 (100) and v2
  (200) envelopes, OCS failure (996 + plugin status), invalid guest JSON →
  502, MountRoutes with two plugins plus unregistered path → 404.
- `go test ./...` green, race clean, golangci-lint 0 issues.
