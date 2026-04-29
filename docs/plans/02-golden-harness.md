# Golden Test Harness — Design

Status: Draft v1 (Phase 0)
Owner: Phase 0 working group
Related: `docs/plans/01-phase-0-blueprint.md`, `docs/adr/0001-stable-contracts.md`

## 1. Purpose

Wire-compatibility with the official Nextcloud desktop, iOS, Android, CalDAV, and CardDAV clients is a **non-negotiable** Phase 0–4 acceptance gate. Reading the PHP source is necessary but not sufficient: the implementation drift between documented behavior and observed behavior is the principal risk. The golden harness exists to:

1. Capture **observed** HTTP/WebDAV exchanges from the reference PHP server as the canonical specification.
2. Persist captures as version-controlled, human-reviewable **golden cases** under `testdata/golden/`.
3. Replay cases against the Go implementation in CI and assert byte-equivalence after a defined normalization pass.
4. Surface the smallest possible diff when a regression is introduced.

Non-goals (Phase 0):
- Performance benchmarking (separate harness, Phase 2).
- Browser/UI testing (out of scope for the wire contract).
- Mutating end-to-end flows that require persistent cluster state (Phase 1+).

## 2. Capture Source of Truth

| Source | Use | Notes |
|---|---|---|
| `mitmproxy` HAR export | Primary | Run reference PHP stack via `deploy/docker/docker-compose.dev.yml` (Phase 1), proxy real clients through `mitmdump -w session.har`. |
| Hand-authored YAML | Secondary | For deterministic cases where capture is awkward (error paths, malformed input). Must be marked `synthetic: true`. |
| Existing OCS/WebDAV docs | Tertiary | Used to **explain** captures, never to replace them. |

Phase 0 exit gate requires **≥50** captured cases, of which **≥10** must be `replayable: true` (see §6).

## 3. Directory Layout

```
testdata/golden/
  README.md                           # human-facing index + authoring guide
  _schema/
    case.schema.json                  # JSON Schema for case.yaml
    normalize.md                      # normalization rules, versioned
  status/
    001-status-php-anonymous/
      case.yaml                       # metadata + assertions
      request.http                    # raw HTTP/1.1 request bytes
      response.http                   # raw HTTP/1.1 response bytes
      notes.md                        # optional: capture context, client version
  ocs/
    capabilities/
      001-anonymous/
      002-authenticated-basic/
  webdav/
    propfind/
      001-root-depth-0/
      002-root-depth-1/
    put/
      001-small-file-create/
  caldav/
  carddav/
```

Rules:
- One leaf directory per case. Directory name is the case ID (kebab-case, numeric prefix preserves capture order).
- `request.http` and `response.http` are the raw wire bytes (CRLF line endings, exact body). Stored as committed binary via `.gitattributes` `* -text` for these paths.
- `case.yaml` is the only file the runner parses for metadata; everything else is bytes.

## 4. `case.yaml` Schema (v1)

```yaml
id: ocs/capabilities/001-anonymous
schema_version: 1
captured_at: 2026-04-15T10:22:00Z
captured_from:
  server_version: "30.0.2"
  client: "mitmproxy/10.3.0"
  notes: "anonymous GET /ocs/v2.php/cloud/capabilities"
synthetic: false                       # true if hand-authored
replayable: true                       # see §6
tags: [ocs, capabilities, anonymous, phase-1]

request:
  method: GET
  path: /ocs/v2.php/cloud/capabilities
  headers_strict: [Accept, OCS-APIRequest]   # headers that MUST match
  body_kind: none                            # none | bytes | json | xml | form

response:
  status: 200
  headers_strict: [Content-Type]
  body_kind: xml                             # none | bytes | json | xml | html | dav-multistatus
  normalize:
    - drop_headers: [Date, Server, X-Request-Id, Set-Cookie]
    - replace_header: { name: ETag, with: "<etag>" }
    - json_pointer_redact: ["/ocs/data/version/string"]
    - xml_xpath_redact: ["//d:getlastmodified", "//d:getetag"]
    - dav_multistatus_sort_by_href: true
  assertions:
    - kind: status_eq
    - kind: headers_subset
    - kind: body_equal_after_normalize
```

Reserved future fields: `auth`, `fixtures`, `prereq_cases`, `expected_diagnostics`. Adding fields bumps `schema_version`.

## 5. Normalization Pipeline

Determinism is the only thing that makes byte-equivalence a usable signal. The normalizer runs identically against the captured response and the live response before diffing.

Ordered passes (each opt-in via `case.yaml`):

1. **Header allowlist filter** — drop `drop_headers`, lowercase header names, sort by name.
2. **Header value substitution** — apply `replace_header` (literal placeholder, e.g. `<etag>`).
3. **Body parse**:
   - `json` → canonical JSON (sorted keys, no insignificant whitespace).
   - `xml` / `dav-multistatus` → c14n-lite: sort attributes, strip insignificant whitespace, sort `<d:response>` blocks by `<d:href>` if `dav_multistatus_sort_by_href`.
   - `bytes` → no-op.
4. **Field-level redaction** — `json_pointer_redact`, `xml_xpath_redact` replace matched values with `"<redacted>"`.
5. **Re-serialize** to canonical form for diff.

Non-goals:
- No "fuzzy" body matching (regex over body). If a field is volatile, redact it.
- No status-class collapsing (`2xx`); status must match exactly.

The normalization rules are versioned in `_schema/normalize.md`. Behavior changes require bumping `schema_version` and a migration pass over existing cases.

## 6. Replayability Tiers

Not every captured case can be replayed deterministically against a fresh server. We distinguish three tiers:

| Tier | `replayable` | Meaning | Phase 0 gate |
|---|---|---|---|
| Spec | `false` | Stored as a contract reference; CI only validates schema + normalization round-trips. | Counts toward 50 |
| Replayable | `true` | CI boots the Go server with a known fixture, replays the request, asserts normalized equivalence. | ≥10 required |
| Mutating | `true` + `prereq_cases` | Runs after listed cases mutate state (Phase 1+). | Not required Phase 0 |

Replayable cases are the budget item. Each one needs:
- A known fixture (empty DB, seeded user, seeded file tree). Fixtures live under `testdata/fixtures/` and are referenced by case (Phase 1 work).
- Stable inputs (no clock-dependent bodies that the normalizer cannot redact).

Phase 0 picks the 10 from: `status.php`, `/ocs/v2.php/cloud/capabilities` (anon + basic), `PROPFIND /` depth 0/1 on empty root, `OPTIONS /remote.php/dav/`, anonymous 401 on protected paths, malformed-OCS error envelope, `GET /index.php/login` (HTML smoke), `HEAD /` (server header shape).

## 7. Runner Architecture

```
internal/goldentest/
  case.go         # Case struct, Load(dir) (*Case, error)
  normalize.go    # normalization pipeline, pure functions
  diff.go         # unified diff over normalized bytes, header-aware
  runner.go       # Runner.Run(t, c, target) — drives http.Handler or live URL
  doc.go
```

Public surface (Go):
- `goldentest.Load(dir string) (*Case, error)` — strict YAML + raw-bytes load.
- `goldentest.Normalize(c *Case, resp *http.Response, body []byte) ([]byte, http.Header, error)`
- `goldentest.Diff(want, got []byte, wantH, gotH http.Header) string` — empty string = match.
- `goldentest.RunHandler(t *testing.T, c *Case, h http.Handler)` — in-process replay.
- `goldentest.RunHTTP(t *testing.T, c *Case, baseURL string)` — out-of-process replay (Phase 1+, against compose stack).

Test integration: a single `TestGolden` in each package walks its owned subtree under `testdata/golden/` and calls the runner. CI runs them all; `go test -run TestGolden/ocs` gives focused dev iteration.

Failure UX: on mismatch the runner writes `case.actual.http` next to `response.http` and prints the unified diff. `make golden-accept` (Phase 1) overwrites the golden after human review.

## 8. Capture Tooling — `tools/golden-gen`

Single Go binary, three subcommands. Phase 0 implements only `import-har` skeleton (stub commit), full implementation in the capture sprint.

```
golden-gen import-har \
  --har session.har \
  --out testdata/golden \
  --tag phase-1 \
  --filter 'path:^/ocs/'

golden-gen lint testdata/golden     # schema + normalization round-trip
golden-gen accept testdata/golden/<id>   # promote case.actual.http -> response.http
```

`import-har` responsibilities:
- Parse mitmproxy HAR.
- Group entries by `(method, path-template)` and assign numeric prefixes per directory.
- Emit `request.http`, `response.http` byte-exact.
- Synthesize `case.yaml` with safe defaults (`replayable: false`, common normalization preset by content-type).
- Refuse to overwrite existing cases without `--force`.

## 9. CI Integration

- New job `golden` in `.github/workflows/ci.yml` (Phase 1):
  - `go run ./tools/golden-gen lint testdata/golden`
  - `go test ./... -run TestGolden -count=1`
- Phase 0 CI runs only `golden-gen lint` (no replayable cases yet).
- Coverage of `internal/goldentest` itself ≥80% (it is test infrastructure; bugs here corrupt every downstream signal).

## 10. Risks & Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Capture sprint slips (TLS pinning on mobile clients) | Phase 0 gate miss | Start with desktop client (no pinning); document Android Network Security Config bypass via debug build; iOS deferred to Phase 2 |
| Normalization too lax → false greens | Silent regressions | Default to **strict**; opt-in to redaction per field; `golden-gen lint` round-trips every case |
| Normalization too strict → false reds | Dev friction | Per-case overrides; `make golden-accept` workflow with mandatory PR review |
| Schema churn invalidates cases | Re-capture cost | `schema_version` field + migration script in `tools/golden-gen migrate` |
| Binary blobs bloat repo | Slow clones | PROPFIND/PUT bodies kept small in Phase 0; large-body cases use Git LFS from Phase 2 |
| Captures leak credentials | Security incident | `import-har` strips `Authorization`, `Cookie`, `OC-*-Token` by default; lint fails on residuals |

## 11. Phase 0 Deliverables (this design's exit criteria)

- [x] This design doc committed.
- [ ] `testdata/golden/README.md` + `_schema/` populated.
- [ ] `internal/goldentest` package compiles with API surface above (stubs OK; one happy-path test).
- [ ] `tools/golden-gen` accepts subcommand routing; `lint` works on empty tree.
- [ ] One real case authored end-to-end (`status/001-status-php-anonymous`) as the format reference, marked `replayable: false` until Phase 1 server exists.

## 12. Open Questions

1. Do we need a separate harness for **server-sent events** / long-lived `PROPFIND` with `Depth: infinity`? (Likely yes, Phase 2 — out of scope here.)
2. CalDAV/CardDAV `REPORT` bodies use namespaced XML extensively — confirm c14n-lite handles `xmlns` prefix renaming correctly before declaring Phase 2 ready.
3. Push-notification `notify_push` WebSocket frames — separate harness or extend? (Defer to Phase 3.)
