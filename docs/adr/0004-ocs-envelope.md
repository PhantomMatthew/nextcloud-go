# ADR-0004: OCS Envelope and Status Code Mapping

- Status: Accepted
- Date: 2026-04-29
- Deciders: nextcloud-go core
- Supersedes: —

## Context

Nextcloud's Open Collaboration Services (OCS) API is the primary integration
surface for desktop, iOS, Android, and third-party clients. Two versions are
deployed simultaneously at `/ocs/v1.php` and `/ocs/v2.php`. They share a
response envelope but diverge in how OCS status codes map to HTTP status
codes. Wire compatibility requires byte-stable behaviour across both versions
in JSON and XML formats.

The upstream behaviour is implemented in:

- `lib/private/AppFramework/OCS/BaseResponse.php` (envelope rendering)
- `lib/private/AppFramework/OCS/V1Response.php` (V1 status mapping)
- `lib/private/AppFramework/OCS/V2Response.php` (V2 status mapping)
- `lib/private/AppFramework/Middleware/OCSMiddleware.php` (format negotiation,
  exception coercion, V1/V2 dispatch via script-name suffix)
- `lib/public/AppFramework/OCSController.php` (well-known status constants)
- `lib/public/AppFramework/OCS/OCS{,BadRequest,Forbidden,NotFound}Exception.php`

## Decision

The Go implementation will treat the OCS envelope and status mapping as a
**verbatim wire contract**, not a re-derivation. Concretely:

### Envelope shape

```
{ "ocs": { "meta": { "status", "statuscode", "message",
                     "totalitems", "itemsperpage" },
           "data": <payload> } }
```

XML uses the same field names, root element `<ocs>`, with `<data>` as a
container. `totalitems` and `itemsperpage` are emitted as strings in upstream
output and we will mirror that exactly.

#### Rendering rules (verified against regenerated golden fixtures)

- **Empty meta fields**: `totalitems` and `itemsperpage` are omitted entirely
  from both JSON and XML when the caller does not set them. Upstream PHP
  serializes them only when populated; emitting empty strings would diverge.
- **V1 success code**: `meta.statuscode` defaults to `100` when unset; V2
  defaults to `200`. Default message is `"OK"`.
- **JSON encoding**: produced via `encoding/json.Encoder` with
  `SetEscapeHTML(false)` so `<`, `>`, `&` are not escaped to `\u003c` etc.
  After encoding, every `/` is unconditionally rewritten to `\/` to match
  PHP's `json_encode` default (no `JSON_UNESCAPED_SLASHES` flag). The
  trailing newline appended by `Encoder.Encode` is stripped.
- **JSON booleans**: rendered as native `true`/`false`.
- **XML booleans**: rendered as `"1"` / `"0"` to match PHP's
  `(string)(bool)$value` cast (e.g. `<reference-api>1</reference-api>`).
- **XML preamble**: `<?xml version="1.0"?>\n` — no encoding attribute, single
  newline, no standalone declaration.
- **XML indentation**: one space per level, `\n` line terminators inside the
  body. (HTTP header line endings remain CRLF per RFC 7230.)
- **Ordered keys**: payload key order is caller-controlled via the
  `OrderedMap`/`KV` types; Go's map iteration order is never observed in
  rendered output. This is required for byte-stable golden comparisons.

### Status mapping (verified against upstream source)

OCS-level constants (`lib/public/AppFramework/OCSController.php`):

| Constant                 | Value |
|--------------------------|-------|
| `RESPOND_UNAUTHORISED`   | 997   |
| `RESPOND_SERVER_ERROR`   | 996   |
| `RESPOND_NOT_FOUND`      | 998   |
| `RESPOND_UNKNOWN_ERROR`  | 999   |

V1 (`/ocs/v1.php`) HTTP status:

- `RESPOND_UNAUTHORISED` (997) → HTTP 401
- everything else → HTTP 200

V1 OCS status (envelope `meta.statuscode`):

- HTTP 200 → OCS 100
- otherwise → passthrough

V2 (`/ocs/v2.php`) HTTP status:

- `RESPOND_UNAUTHORISED` (997) → 401
- `RESPOND_NOT_FOUND` (998)   → 404
- `RESPOND_SERVER_ERROR` (996) → 500
- `RESPOND_UNKNOWN_ERROR` (999) → 500
- `code < 200 || code > 600`   → 400
- otherwise → passthrough

### Format negotiation

`OCSMiddleware::getFormat`:

1. `?format=` query/body parameter
2. else `Accept` header, default `xml`

XML is the default when nothing matches. JSON is selected by `format=json` or
an `Accept: application/json` header.

### Exception coercion

- `OCSException` thrown from a controller is converted into a V1/V2 response
  by `OCSMiddleware::afterException`. A `code === 0` becomes
  `RESPOND_UNKNOWN_ERROR` (999).
- A non-OCS controller response with HTTP 401 or 403 is rewrapped as an OCS
  envelope with `RESPOND_UNAUTHORISED` or `403` respectively.

### Header enforcement

The `OCS-APIRequest: true` header is referenced by `OCSController` in its CORS
allow-list (`lib/public/AppFramework/OCSController.php:64`) and is the
canonical bypass for the CSRF check inside `SecurityMiddleware`
(`lib/private/AppFramework/Middleware/Security/SecurityMiddleware.php:208–210`).
For Phase 1 we replicate the bypass: state-changing requests carrying
`OCS-APIRequest: true` skip CSRF token verification but remain subject to
session and Basic Auth checks.

## Consequences

- The Go OCS layer (`internal/ocs`) gets an explicit `Version` enum
  (`V1`, `V2`) and a `Map(version, ocsCode) -> httpCode` function with the
  table above hard-coded. No general-purpose mapping table.
- `Render(version, format, payload, message)` produces the byte-stable
  envelope. Golden tests pin both JSON and XML output.
- `Coerce(error) -> Envelope` mirrors `OCSMiddleware::afterException`,
  including `code === 0 → 999`.
- `OCS-APIRequest` is enforced as a CSRF bypass token in `internal/httpx`'s
  security middleware, not in the OCS layer.
- Any future drift from upstream behaviour requires a new ADR; we do not
  "improve" the envelope.

## Change Log

- 2026-04-30: Amended Decision §Envelope shape with explicit rendering rules
  (empty-meta omission, V1=100/V2=200 default success codes, JSON
  `SetEscapeHTML(false)` + unconditional slash escape, XML bool→`"1"`,
  XML preamble + one-space indent + LF body, OrderedMap-driven key order).
  Documents behaviour pinned by `internal/ocs/ocs_test.go` golden fixtures
  `001-anonymous-v1` (XML, body LF) and `002-anonymous-v2-json` (JSON,
  regenerated to PHP-default output; original was hand-authored and bugged).

## References

- Upstream PHP files listed in Context.
- `docs/plans/03-phase-1-blueprint.md` §3.
- Phase 0 golden harness (`docs/plans/02-golden-harness.md`).
