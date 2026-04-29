# Golden Cases

Wire-contract fixtures captured from the reference Nextcloud PHP server and
replayed against this Go implementation. See
[`docs/plans/02-golden-harness.md`](../../docs/plans/02-golden-harness.md) for
the full design.

## Layout

```
testdata/golden/
  README.md                       # this file
  _schema/
    case.schema.json              # JSON Schema for case.yaml (v1)
    normalize.md                  # versioned normalization rules
  <area>/<group>/<NNN>-<slug>/
    case.yaml                     # metadata + assertions
    request.http                  # raw HTTP/1.1 request bytes (CRLF)
    response.http                 # raw HTTP/1.1 response bytes (CRLF)
    notes.md                      # optional capture context
```

Areas in scope for Phase 0–2: `status`, `ocs`, `webdav`, `caldav`, `carddav`.

## Authoring a case

1. Capture with `mitmproxy` against the reference PHP stack:
   `mitmdump -w /tmp/session.har`.
2. Import: `go run ./tools/golden-gen import-har --har /tmp/session.har --out testdata/golden`.
3. Inspect the generated `case.yaml`; tighten `headers_strict` and `normalize`.
4. Run `go run ./tools/golden-gen lint testdata/golden`.
5. If replayable in Phase 0, set `replayable: true` and add to the runner.

Hand-authored cases are allowed for deterministic edge paths; set
`synthetic: true` and document why in `notes.md`.

## Reviewing a case

- `request.http` / `response.http` are wire bytes. Treat them as the
  specification — do not reformat.
- `case.yaml` is reviewed as code. Changes to `normalize` or `assertions`
  require a justification in the PR description.
- Promoting a regenerated response (`make golden-accept ID=...`) requires a
  separate commit; the diff must be reviewed by a human, not auto-merged.

## Phase 0 exit gate

- ≥50 cases total across the areas above.
- ≥10 cases with `replayable: true`.
- `go run ./tools/golden-gen lint testdata/golden` exits 0 in CI.
