# ADR-0038: Phase 4b Plugin Packaging, Signing & Install

- **Status**: Accepted
- **Date**: 2026-09-21
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

Phase 4a (ADR-0037) delivered the runtime core but plugins could only be
loaded from loose files (`plugin check`). The spec requires `.ncplugin`
archives (§4), ed25519 signature verification at install (§13), and leaves
the signing trust model open (§14 Q2).

## Decision

1. **Archive.** `.ncplugin` is a zip with `plugin.toml`, the manifest's
   module wasm, and optional extras (`i18n/`, README, settings schema).
   Limits: archive ≤ 128 MiB, module ≤ 64 MiB, ≤ 256 entries; `..` member
   names skipped. `signature.sig` is reserved.

2. **Signature format.** The signed payload is the canonical JSON array of
   `{name, sha256}` for every member (plugin.toml raw bytes included),
   sorted by name. `signature.sig` is JSON `{keyid, signature}` where
   `keyid` = first 16 hex chars of SHA-256(public key) and `signature` =
   base64 ed25519 over the payload. Key files are single-line base64:
   `<name>.key` (64-byte private, mode 0600), `<name>.pub` (32-byte
   public).

3. **Trust model (spec §14 Q2 resolved for v1): operator-pinned keys.**
   Trusted public keys live in `<install_dir>/trusted_keys/*.pub`. An
   archive signed by an untrusted key is refused; an unsigned archive is
   refused unless the admin passes `--force-unsigned`. No CA, no TUF
   delegation — the marketplace question stays out of v1.

4. **Registry.** Migration `0015_plugins` (three dialects): id PK,
   version, enabled, capabilities_json snapshot, signature_keyid,
   archive_path, installed/updated timestamps. MySQL needs the
   `ON DUPLICATE KEY UPDATE` variant; sqlite/postgres use
   `ON CONFLICT (id)`.

5. **Install flow.** `Installer.Install`: verify signature → compile
   check (`Host.Load`) → on upgrade compare the stored capability snapshot
   (spec §13 "capability changes on upgrade require admin re-approval")
   and refuse with `ErrCapsChanged` unless `--approve-caps` → store
   archive at `<install_dir>/<id>/<version>.ncplugin` (mode 0600) → run
   `on_install` → registry upsert. Failure after the archive write removes
   the file. `Uninstall` runs `on_uninstall` best-effort (failures logged,
   not fatal), then deletes the record and the plugin directory.

6. **Boot loading.** `Plugin.Start` (new) warms instances and checks the
   ABI *without* lifecycle hooks; `Install` = `Start` + `on_install`. At
   app boot `StartEnabled` loads every enabled plugin; per-plugin failures
   are logged and skipped so a broken plugin cannot take down the server.
   App shutdown closes plugins before the host.

7. **CLI.** `ncgo-cli plugin`: `keygen`, `pack`, `sign`, `verify`,
   `install`, `uninstall`, `list`, `enable`, `disable` (plus the existing
   `check`).

## Alternatives Considered

### Self-hosted CA / TUF-style delegation
- Pros: key rotation, publisher identity.
- Cons: infrastructure far beyond v1 needs; operator-pinned keys give the
  same "only code I trust runs" guarantee for self-hosted installs.

### Store the wasm in the database
- Pros: single backup artifact.
- Cons: multi-MB blobs in the registry table; the filesystem install dir
  already exists in config (`plugin.install_dir`).

### Fail boot when an enabled plugin fails to start
- Pros: surfaces misconfiguration loudly.
- Cons: one corrupt plugin would DoS the whole instance; logged-and-skipped
  matches how operators expect app stores to behave.

## Consequences

- Any party with write access to `trusted_keys/` controls what code the
  server runs; filesystem permissions on the install dir are part of the
  security boundary (documented for operators).
- Downgrade installs (lower version string) are allowed; version ordering
  is informational in v1.
- Follow-ups: hot-reload, marketplace/discovery (spec §14 Q4), signature
  on the settings schema enforcement, Prometheus metrics (spec §12).

## Verification

- Unit: archive round-trip + zip-bomb guards, sign/verify incl. tamper
  and untrusted-key rejection, registry CRUD (sqlite), installer flows
  (signed, unsigned policy, upgrade caps gate, uninstall).
- CLI smoke (real binary, sqlite): keygen → pack → sign → verify →
  install → list → disable → enable → uninstall; unsigned archive refused
  without `--force-unsigned`, accepted with it.
- `go test ./...` green, race clean, golangci-lint 0 issues; total
  coverage 68.0% (new CLI surface is smoke-tested, not unit-tested).
