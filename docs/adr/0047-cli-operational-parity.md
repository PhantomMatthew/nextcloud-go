# ADR-0047: Phase 4d2 ncgo-cli Operational Parity (user/group/config commands)

- **Status**: Accepted
- **Date**: 2026-09-22
- **Deciders**: Project lead
- **Supersedes**: (none)

## Context

`ncgo-cli` is the occ-equivalent operator CLI. Before this increment it
covered only `migrate`, `user add`, and the `plugin` tool set; routine
operations (listing users, disabling accounts, resetting passwords, managing
groups, editing app/plugin configuration) required direct SQL. Phase 4d2
closes the gap for the three most common occ command families:
`user:*`, `group:*`, and `config:*` (app-scoped).

## Decision

1. **Command surface.** New subcommands, all reading the standard `--config`
   flag and opening the configured database:
   - `user list [--limit] [--offset]` (tabular: uid, display name, enabled,
     matching `plugin list` conventions), `user enable|disable <uid>`,
     `user delete <uid> --yes`, `user reset-password <uid> --password-stdin`
     (same Argon2id parameters as `user add`, from server config).
   - `group list [--limit] [--offset]`, `group add <gid> [--display-name]`,
     `group delete <gid> --yes`, `group adduser|removeuser <gid> <uid>`,
     `group members <gid> [--limit]` (one uid per line, script-friendly).
   - `config get|set|delete <appid> <key> [<value>]` over the `appconfig`
     table added in Phase 4c6. `config get` on an unset key prints nothing
     to stdout and exits 1 (documented in `--help`), so it is safe in shell
     scripts. Plugin configuration is addressed as appid `plugin` with key
     `<plugin_id>.<key>`, documented in the command's long help (this is the
     v1 mechanism the 4d webhook-forwarder README referenced).

2. **Store additions (`internal/users`).** `SetEnabled`, `Delete`,
   `List`, `GroupMembers`, `RemoveGroupMember`, `DeleteGroup`, `ListGroups`
   on `SQLStore` and the `Store` interface. A dedicated `List` was added
   rather than reusing `Search`: `Search` filters to enabled users only,
   caps at 20, and returns nil for an empty term — none of which an
   operator listing wants. Row scanning was factored into shared helpers
   (`scanUserRow`/`scanUsers`) used by `GetBy*`, `Search`, and `List`.

3. **Delete semantics: no cascade beyond memberships.** `user delete`
   removes the user row and their `group_members` rows only; files, shares,
   sessions, and other owned data are deliberately NOT cascaded — purging or
   reassigning them is the operator's responsibility, mirroring the warnings
   occ prints for `user:delete`. The command long help states this. Likewise
   `group delete` removes the group row and its memberships, nothing else.

4. **`--yes` confirmation convention.** Destructive commands
   (`user delete`, `group delete`) require an explicit `--yes` flag and
   error out otherwise. The repo has no TTY-prompt precedent
   (`plugin uninstall` is unconditional), so no interactive prompt was
   added; a prompt can be layered on later without changing the flag
   contract.

5. **Error conventions.** Unknown uid/gid maps from the store's
   `users.ErrNotFound` (via `errors.Is`) to a clear
   `ncgo-cli: unknown user|group "..."` message and a non-zero exit.
   `users.ErrExists` from `user add`/`group add` propagates unchanged.

6. **Boilerplate factoring.** The repeated `config.Load` + `database.Open`
   sequence moved to `loadConfig()`/`openDB()` helpers in
   `cmd/ncgo-cli/deps.go`; `migrate.go`'s `withDB` now builds on them.
   `cmd/ncgo-cli/cli_test.go` establishes the (previously missing) CLI test
   pattern: a temp sqlite DB migrated in-process, a minimal config file,
   and the cobra tree executed via `newRoot()` with captured output.

7. **Enabled is enforced at login.** No auth changes were needed:
   `internal/users/verifier.go` (password verify), `internal/auth/bearer.go`
   (app-password/session-token lookup), and
   `internal/auth/middleware.go` (session lookup) already reject disabled
   accounts, so `user disable` blocks new logins and existing session
   resolution immediately.

## Alternatives Considered

### Reusing `Search` for `user list`
- Pros: no new store method.
- Cons: `Search` excludes disabled users (exactly the ones an operator
  needs to see after `user disable`), caps results at 20, and treats an
  empty term as "no query". A separate `List` is clearer and cheap.

### Interactive confirmation prompt instead of `--yes`
- Pros: closer to occ's `user:delete` UX.
- Cons: no prompt precedent in the codebase; prompts complicate scripting
  and tests. `--yes` is explicit, scriptable, and extensible to a prompt
  later.

### Cascading user delete into files/shares/sessions
- Pros: one-step account teardown.
- Cons: data-loss blast radius is far larger than occ's own default; the
  files/shares ownership model (reassignment vs purge) deserves its own
  design pass. Explicitly deferred.

## Consequences

- The occ command families covered as of this ADR: `user` (add, list,
  enable, disable, delete, reset-password), `group` (list, add, delete,
  adduser, removeuser, members), `config` (app-scoped get/set/delete,
  including plugin config), plus the pre-existing `plugin` and `migrate`
  sets.
- **Explicitly deferred** occ families: maintenance-mode toggle (today
  driven by the `maintenance.enabled` config file value; a runtime toggle
  needs an appconfig-backed flag read by the server, not just a CLI
  writer), background-job inspection (`jobs` table exists; no read
  surface), app-password management, theming/branding.
- The `users.Store` interface grew by seven methods; the one test stub in
  the repo (`internal/web` login v2 tests) was extended accordingly.
- `cmd/ncgo-cli` now has a test harness pattern other CLI increments can
  copy.

## Verification

- `cmd/ncgo-cli/cli_test.go`: end-to-end against temp sqlite — user
  add/list/enable/disable/reset-password/delete (including missing-user
  errors and the `--yes` requirement), group
  add/list/adduser/members/removeuser/delete, config set/get/delete
  (including unset-key exit-1-with-empty-stdout), and post-reset password
  verification through the real Argon2id verifier.
- `internal/users/users_test.go`: store-level tests for all new methods
  (ordering, pagination, membership cascade on delete, not-found paths).
- `go build ./...`, `go test ./...` green, `go test -race` clean,
  `golangci-lint run ./...` 0 issues, `go mod tidy` no diff, total coverage
  75.5% (gate ≥ 74.9%).
- Manual smoke run of the built binary: migrate → add users → disable →
  list → group ops → config round-trip → delete, all with expected output
  and exit codes.
