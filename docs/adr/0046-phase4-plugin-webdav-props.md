# ADR-0046: Phase 4c8 Plugin WebDAV Properties (webdav_register_prop + live props)

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Spec §6.3 defines `ncgo.webdav_register_prop(name, getter_name, setter_name)`
gated by the `webdav.props` capability, and §7 sketches a plugin-defined
getter `(resource_path_ptr, len) -> i64` returning a MessagePack value. Phase
4a registered `webdav_register_prop` as a stub returning `ErrUnsupported`
after the capability check. This increment lands the real implementation:
hook-time registration persisted in a new `plugin_webdav_props` table, and
live plugin-provided properties on the files WebDAV PROPFIND/PROPPATCH path.

## Decision

1. **Per-plugin namespace URI.** Every plugin prop is emitted under the
   namespace `http://ncgo.local/ns/plugin/<plugin_id>` with the local name
   being the part after `:` in the registered `prefix:local` name. Two
   plugins registering the same local name (even under different prefixes)
   therefore never collide on the wire, and PROPPATCH can route a write back
   to the owning plugin by parsing the plugin id out of the namespace. The
   registered prefix is a manifest/capability namespacing device only; it
   does not appear on the wire.

2. **Raw-string getter/setter convention (deviation from spec §7).** The
   spec's getter returns an i64-packed MessagePack value. v1 instead uses:
   - getter `(path_ptr, path_len, out_ptr, out_max) -> i32` — the host
     allocates a 4 KiB out buffer via `ncgo_alloc`, the guest writes the
     **raw string value** into it and returns the byte count (0 = empty
     value, negative = error code).
   - setter `(path_ptr, path_len, val_ptr, val_len) -> i32` — 0 = OK,
     anything else maps to HTTP 500.
   This matches the existing `(ptr, len, out, max)` idiom used by
   `config_get`, `cache_get`, and the `ncgo_response_*` family, avoids a
   MessagePack round-trip for what is almost always a short string, and
   keeps value escaping entirely on the host (XML-escaped at emission).

3. **Registration semantics.** `webdav_register_prop` is lifecycle-hook
   only (-3 outside hooks), requires a `webdav.props` grant covering the
   name (-3), validates the name as `prefix:local` with a non-empty prefix
   and `local` matching `[A-Za-z][A-Za-z0-9_-]*` (-2), requires a getter
   export name that is identifier-ish `[A-Za-z_][A-Za-z0-9_.]*` ≤ 128 (-2),
   accepts an **empty setter** (read-only property), and persists to
   `plugin_webdav_props` (migration `0018`, PK `(plugin_id, name)`;
   nil registry → -12). Uninstall deletes the rows alongside routes.

4. **Live-prop plumbing, not persistence.** Plugin props are computed, not
   stored: `webdav.LivePropProvider` (in `internal/webdav`, because
   `internal/plugins` already imports `internal/files` and cannot be
   imported back) is implemented by `plugins.PropProvider` and attached to
   the files `DAV` at boot after `StartEnabled`. `Stat`/`List`/`Read`
   attach `PropsFor` results as `Entry.ExtraProps`; `writeProps` emits each
   as `<x:NAME xmlns:x="NS">value</x:NAME>` (inline xmlns, SabreDAV style;
   empty NS/Name skipped). PROPPATCH gives the provider **first refusal**:
   `handled=false` falls through to the existing `oc:favorite` logic;
   handled ops report the provider's status (200 OK, 403 for a read-only
   prop or a detached plugin, 500 on guest failure). A remove op is
   delivered to the setter as an empty value. The PROPPATCH result
   multistatus emits unknown namespaces with the same inline-xmlns shape.

5. **Failure isolation.** A PROPFIND must never fail because a plugin
   misbehaved: getter errors, traps, missing plugins (detached since
   install), and registry list failures omit the prop (debug/warn log) and
   the response is still produced. Only attached plugins are invoked — the
   provider resolves records against the host's event-dispatch attach set
   via a new `Host.attachedPlugin(id)`. The plugin call identity carries
   the PROPFIND/PROPPATCH user as `CallContext.UserID`, so `ctx_user_id`
   and user-scope storage work inside getters/setters.

## Alternatives Considered

### Persisting prop values in file_properties
- Pros: PROPFIND is a pure SQL read; allprop is cheap; values survive
  plugin restarts.
- Cons: the spec's model is computed props (a getter exists precisely so
  the value can be live); persistence would need write-through semantics,
  invalidation rules, and a value schema, none of which the ABI defines.

### MessagePack values per spec §7
- Pros: matches the letter of the spec; supports structured values.
- Cons: every other string-returning host interaction in the ABI uses raw
  (ptr, len) buffers; WebDAV prop values are strings on the wire anyway, so
  MessagePack buys nothing and costs a decoder in every guest language.
  Documented as a v1 deviation in the spec Change Log.

### Emitting plugin props under their registered prefix namespace
- Pros: the wire namespace would echo the registration.
- Cons: prefixes are not URIs and two plugins may register the same
  `prefix:local`; the per-plugin URI scheme is collision-free by
  construction.

## Consequences

- Guest-visible contract: `pluginsdk.WebDAVRegisterProp(name, getter,
  setter)` (empty setter = read-only) and `pluginsdk.WebDAVPropArgs(...)`;
  the getter/setter exports use the raw-string convention above.
- WebDAV responses gain `<x:* xmlns:x="http://ncgo.local/ns/plugin/<id>">`
  elements whenever a live plugin provides them; `ParseMultistatus` already
  tolerates unknown elements (covered by test).
- Detached plugins' prop rows survive in the table and simply stop being
  emitted; a restart re-attaches and revives them.
- Follow-ups: allprop performance (a getter call per prop per entry —
  consider batching or a per-request memo), prop value caching, and a
  proper 404 propstat for requested-but-unset plugin props (a getter
  currently returning empty is indistinguishable from unset).

## Verification

- `webdav_register_prop` through wasm guests: outside hook → -3; no
  capability → -3; bad name forms (no colon, empty prefix/local, bad local
  charset) → -2; empty/invalid/overlong getter, invalid setter → -2; nil
  registry → -12; success → 0 with the row persisted; empty setter →
  read-only row.
- Registry round-trip (upsert replaces, per-plugin and global listing,
  delete) and uninstall cascade.
- End-to-end PROPFIND on a real files DAV (sqlite + localfs) with a started
  wasm plugin: the multistatus contains
  `<x:tags xmlns:x="http://ncgo.local/ns/plugin/<id>">prop-value</x:tags>`;
  getter error omits the prop and the response stays 207-able; two plugins
  with the same local name appear under different xmlns.
- End-to-end PROPPATCH: set → guest logs `setprop <path> <value>` and the
  result is 200; remove → setter sees an empty value; read-only → 403;
  unknown prop → falls through to the old behavior; detached plugin → prop
  omitted / 403.
- `go test ./...` green, race clean, golangci-lint 0 issues,
  `GOOS=wasip1 GOARCH=wasm go build ./pkg/pluginsdk/` clean.
