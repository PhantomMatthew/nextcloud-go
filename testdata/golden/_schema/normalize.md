# Normalization Rules

Schema version: **1**

The normalizer runs identically against the captured response and the live
response before diffing. Order matters; passes execute top to bottom. Each
pass is opt-in via the case's `response.normalize` list.

## Pass 1 — Header allowlist filter

- `drop_headers: [Name, ...]` — remove headers whose name matches
  case-insensitively. Always implicitly drops `Date` for status responses
  (override by listing it in `headers_strict`).
- After dropping, header names are lowercased and the set is sorted by name
  before comparison.

## Pass 2 — Header value substitution

- `replace_header: { name: ETag, with: "<etag>" }` — replace the entire
  value of `name` with the literal `with`. Used for opaque server-generated
  identifiers that must be present but whose value is non-deterministic.

## Pass 3 — Body parsing

Selected by `response.body_kind`:

| `body_kind`        | Parser                                                                 |
|--------------------|------------------------------------------------------------------------|
| `none`             | Body must be empty.                                                    |
| `bytes`            | No parsing; byte-equal compare.                                        |
| `json`             | Decode → re-encode with sorted keys, no insignificant whitespace.      |
| `xml`              | c14n-lite: sort attributes, strip insignificant whitespace.            |
| `html`             | Parse with `golang.org/x/net/html`, re-serialize canonical form.       |
| `dav-multistatus`  | As `xml`, plus sort `<d:response>` blocks by `<d:href>` if requested.  |

## Pass 4 — Field-level redaction

- `json_pointer_redact: ["/path/to/field", ...]` — replace matched values
  with the literal string `"<redacted>"`. Applied after JSON canonicalization.
- `xml_xpath_redact: ["//d:getlastmodified", ...]` — replace text content of
  matched element(s) with `<redacted>`. Attributes are not touched.

## Pass 5 — Re-serialize

The normalized form is re-emitted in the canonical body shape from Pass 3 and
diffed line-by-line. A non-empty unified diff is a test failure.

## Reserved DAV namespace prefixes

For c14n-lite XPath matching the following prefix bindings are pre-registered:

| Prefix | Namespace                                |
|--------|------------------------------------------|
| `d`    | `DAV:`                                   |
| `oc`   | `http://owncloud.org/ns`                 |
| `nc`   | `http://nextcloud.org/ns`                |
| `s`    | `http://sabredav.org/ns`                 |
| `cal`  | `urn:ietf:params:xml:ns:caldav`          |
| `card` | `urn:ietf:params:xml:ns:carddav`         |

## Versioning

A change to any pass that affects the canonical output bumps `schema_version`.
Existing cases are migrated by `tools/golden-gen migrate`. Cases declaring an
older `schema_version` than the runner supports fail `lint` with an
actionable message.
