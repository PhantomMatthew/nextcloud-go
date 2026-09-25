# ADR-0089: Phase 5p plugin old-version archive GC

- **Status**: Accepted
- **Date**: 2026-09-25
- **Deciders**: Project lead
- **Supersedes**: (none)
- **Amends**: ADR-0056 (closes its old-version archive GC deferral)

## Context

`Installer.Install` stores every verified archive at
`<install_dir>/<id>/<version>.ncplugin`. On upgrade the new file is written
and `Registry.Upsert` re-points the row's `ArchivePath` at it — but the old
version's file is never removed, so the directory grows by one archive per
upgrade, forever. Archives carry the WASM module, so the leak is
non-trivial in size.

ADR-0056 saw this and deliberately deferred it: *"Old-version archives
under `<install_dir>/<id>/` are kept after upgrade (pre-existing behavior;
potential rollback story, no GC yet)."* The deferral named the tension:
the accumulated files are the only rollback artifact, so a sweep must not
destroy the ability to go back one version.

One constraint from ADR-0056 frames the timing: a failing upgrade hook
leaves the OLD archive installed (the new file is removed, the registry
row still points at the old one). Any GC that ran before the install was
known-good could therefore collect the very archive the plugin is still
running from.

## Decision

1. **Keep the current archive plus one previous** — the minimal rollback
   story. After a successful UPGRADE install that changes the version
   (post-Upsert), delete every `*.ncplugin` in `<install_dir>/<id>/`
   except the just-installed `<version>.ncplugin` and
   `<fromVersion>.ncplugin` (the version the registry row pointed at
   before the upgrade). The directory is then bounded at two files per
   plugin.

2. **Post-Upsert, version-changing timing only.** The sweep runs at the
   very end of `Install`, after `Registry.Upsert` succeeds, guarded by
   `if upgrade && version != fromVersion`. Failed installs and failed
   upgrades return before this point and never collect anything — in
   particular the old archive survives every failed upgrade, matching
   ADR-0056's failure semantics. Fresh installs (`upgrade == false`) do
   not GC, and `Uninstall` is untouched (it already `RemoveAll`s the
   whole `<install_dir>/<id>` tree).

3. **Best-effort failure contract.** The sweep is an unexported helper,
   `Installer.gcOldArchives(dir, keepCurrent, keepPrevious)`:
   `os.ReadDir(dir)`, skip subdirectories, skip names not ending in
   `.ncplugin`, skip the two keep names, `os.Remove` the rest. A ReadDir
   or per-file Remove error is Warn-logged through the installer's
   nil-gated logger and never returned — housekeeping must not be
   contagious: a GC hiccup never fails an otherwise-successful install.

4. **Edge versions.** A same-version reinstall (re-signed or rebuilt
   archive, same version string) is a GC no-op: the keep-previous rule
   exists specifically to preserve the rollback story, and a routine
   re-upload must not silently delete the previous distinct version's
   archive — the last rollback artifact. Boundedness is unaffected: the
   directory converges to two files on the next version-changing upgrade.
   A downgrade (installing an older version over a newer one) keeps BOTH
   files: `fromVersion` is the newer one, so the manual rollback state is
   preserved in either direction.

## Alternatives Considered

### Keep all archives (status quo)
- Pros: zero code; every historical version remains available.
- Cons: this is the bug — unbounded per-plugin growth on every upgrade,
  with no operational bound. Rejected.

### Keep current only
- Pros: tightest bound (one file per plugin).
- Cons: loses exactly the rollback story ADR-0056 deferred GC to
  preserve; the first upgrade after this ships would destroy the only
  on-disk rollback artifact. Rejected.

### Manual hygiene via a CLI subcommand (e.g. `plugin gc`)
- Pros: no automatic deletion; operators opt in.
- Cons: operators should not have to run manual hygiene for what can be a
  bounded automatic sweep; a forgotten cron recreates the leak. The
  automatic keep-two rule needs no operator decision. Rejected.

## Consequences

- `<install_dir>/<id>/` is bounded at two archives per plugin after every
  successful version-changing upgrade, regardless of upgrade cadence;
  same-version reinstalls never shrink it below that.
- Manual rollback stays simple: reinstall the previous archive — the file
  is right there in the plugin's directory.
- GC failures are invisible to install success (Warn-log only); a plugin
  can accumulate extra archives only when removals genuinely fail, which
  the logs surface.
- Downgrades preserve both the newer and the older archive, so moving
  forward again or staying back both remain possible.

## Verification

- `internal/plugins/install_test.go`:
  `TestInstallUpgradeArchiveGC` installs v1, upgrades to v2 and v3, and
  asserts the directory holds exactly `2.0.0.ncplugin` + `3.0.0.ncplugin`
  with `1.0.0.ncplugin` collected and the registry row's `ArchivePath` at
  the v3 file; a same-version v3-over-v3 reinstall leaves both archives
  untouched (GC no-op), and a follow-up v3→v4 upgrade converges the
  directory to exactly `4.0.0.ncplugin` + `3.0.0.ncplugin`.
  `TestInstallFreshInstallNoGC` pins the scope: a fresh install of a
  second plugin leaves the first plugin's two-archive directory
  byte-identical. `TestGCOldArchives` unit-tests the helper on a
  synthetic directory — strictly-older archives collected, keep-set and
  non-`.ncplugin` files untouched, a subdirectory with an
  archive-looking name surviving via the IsDir skip, and a missing
  directory Warn-and-returning without panic even with a nil logger.
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` clean (no new
  dependencies).
