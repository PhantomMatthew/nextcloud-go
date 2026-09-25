# File Tagger plugin

Reference plugin implementing the walkthrough in
[`docs/specs/wasm-plugin-abi.md`](../../docs/specs/wasm-plugin-abi.md) §10:
when a user uploads a file whose path ends in `.invoice.pdf`, the plugin
records an `invoice` tag row in its own `file_tags` table.

## What it demonstrates

- Subscribing to the core `files.uploaded` event (emitted on every WebDAV
  write; payload is a msgpack map `{user, path, size, created}`).
- DDL inside a lifecycle hook: `ncgo_on_install` creates the `file_tags`
  table — DDL is denied outside `on_install`/`on_uninstall`.
- Parameterized `db_exec` writes from `ncgo_on_event`, with a cheap
  byte-level pre-filter so non-matching uploads are skipped without
  decoding the payload.

## Capabilities

| Grant | Why |
|---|---|
| `db.read = ["file_tags"]` / `db.write = ["file_tags"]` | The plugin owns exactly one table; globs are enforced per query by the host's SQL parser. |
| `events.subscribe = ["files.uploaded"]` | Receives upload events. The plugin publishes nothing, so no `events.publish` grant. |

Anything not listed is denied — the guest cannot touch any other table or
topic.

## Build and install

Build the guest (requires TinyGo; wasip1 reactor mode — the host admits a
pinned `wasi_snapshot_preview1` subset, ADR-0092):

```bash
make example-plugins        # or: tinygo build -o examples/file-tagger/file-tagger.wasm -target=wasip1 -buildmode=c-shared -no-debug ./examples/file-tagger
```

Pack, sign, install, enable:

```bash
go run ./cmd/ncgo-cli plugin keygen -o /tmp/tagger
go run ./cmd/ncgo-cli plugin pack examples/file-tagger -o /tmp/file-tagger.ncplugin
go run ./cmd/ncgo-cli plugin sign /tmp/file-tagger.ncplugin --key /tmp/tagger.key
cp /tmp/tagger.pub <plugin-install-dir>/trusted-keys/   # see plugin.install_dir in config
go run ./cmd/ncgo-cli plugin install /tmp/file-tagger.ncplugin
go run ./cmd/ncgo-cli plugin enable com.example.file-tagger
# the server picks the change up within plugin.refresh_interval (default 10s);
# a restart works too
```

(`plugin check` is deliberately absent: it smoke-tests compilation and ABI
shape on an empty host — no database, registry, or storage — so hooks that
call host services, like this plugin's `db_exec` DDL, fail there.
`plugin install` runs the hooks with the full install-time host: DB, route/
OCS/WebDAV-prop registry, app config, jobs, event bus, and system storage,
so `on_install` works exactly as on the server. `--force-unsigned` on
install skips the signing steps for local experimentation.)

## Watch it work

Upload a matching file over WebDAV, then read the table back:

```bash
curl -u alice:password -T invoice.pdf \
  http://localhost:8080/remote.php/dav/files/alice/docs/acme.invoice.pdf
sqlite3 data/ncgo.db 'SELECT user, path, tag FROM file_tags;'
# alice|/docs/acme.invoice.pdf|invoice
```

`created_at` is recorded as `0`: ABI v1 exposes no wall clock to
freestanding guests (there is no WASI), so the example writes `0` until a
clock host function lands. The `id INTEGER PRIMARY KEY` column
auto-increments on SQLite; on PostgreSQL/MySQL adjust the `CREATE TABLE`
statement in `main.go` to the dialect's identity flavor
(`GENERATED ... AS IDENTITY` / `AUTO_INCREMENT`), and note that `user` may
need quoting on PostgreSQL.

## Caveats

- The guest imports `github.com/vmihailenco/msgpack/v5` (already a host
  dependency). TinyGo/wasip1 compatibility is verified by
  `make example-plugins` (ADR-0092); `ncgo-cli plugin check` still cannot
  exercise this plugin's DB-touching `on_install` (see above).
