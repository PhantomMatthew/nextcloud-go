# Webhook Forwarder plugin

Reference plugin that forwards every `files.uploaded` event to an HTTP
webhook as a JSON POST, optionally signed with HMAC-SHA256.

## What it demonstrates

- Subscribing to the core `files.uploaded` event and decoding its msgpack
  payload (`{user, path, size, created}`).
- Reading plugin-local config (`webhook.url`, `webhook.secret`) via
  `config_get`; a missing `webhook.url` is **not** an error — the event is
  skipped quietly.
- Outbound HTTP via `http_request` under an exact `host[:port]` grant, with
  a 5s timeout, and closing the response handle.
- Request signing via the `crypto_hmac` host function
  (`X-Signature: hex(hmac-sha256(secret, body))`).
- Failure etiquette: transport failures are logged and swallowed (return
  `0`) so a dead endpoint cannot retry-storm the event bus; only setup
  bugs (denied host, invalid request) return non-zero.

## Capabilities

| Grant | Why |
|---|---|
| `events.subscribe = ["files.uploaded"]` | The trigger topic. |
| `http.outbound = ["localhost:8080"]` | Outbound calls are allowed only to granted `host[:port]` targets (a host-only grant covers the default ports 80/443). **Edit this to your webhook receiver before packing.** |
| `http.outbound_allow_private = true` | `localhost` is a private target that the dial-time SSRF guard (ADR-0057) refuses without this grant. Drop it if your receiver is a public host. |
| `config.read = ["webhook.*"]` | Reads `webhook.url` / `webhook.secret`. The plugin never writes config, so no `config.write` grant. |

## Build and install

```bash
make example-plugins        # requires TinyGo (wasm-unknown); see file-tagger README
go run ./cmd/ncgo-cli plugin check examples/webhook-forwarder
go run ./cmd/ncgo-cli plugin pack examples/webhook-forwarder -o /tmp/webhook-forwarder.ncplugin
go run ./cmd/ncgo-cli plugin install --force-unsigned /tmp/webhook-forwarder.ncplugin
go run ./cmd/ncgo-cli plugin enable com.example.webhook-forwarder
# restart the server: the enabled plugin set is read once at boot
```

(Sign with `plugin keygen` / `plugin sign` like the file-tagger README for a
trusted install.)

## Configuration (v1 mechanism)

There is no `ncgo-cli` command for plugin config yet (follow-up). Until
then, set keys directly in the `appconfig` table — plugin config lives
under `appid = 'plugin'`, namespaced as `<plugin-id>.<key>`:

```sql
INSERT INTO appconfig (appid, configkey, configvalue) VALUES
  ('plugin', 'com.example.webhook-forwarder.webhook.url', 'http://localhost:8080/hook'),
  ('plugin', 'com.example.webhook-forwarder.webhook.secret', 's3cret');
```

Both keys are read per event, so edits take effect without a restart.
`webhook.secret` is optional; when set, requests carry `X-Signature`.

## Watch it work

Run a sink and upload a file:

```bash
nc -l 8080    # or any HTTP echo server
curl -u alice:password -T notes.txt \
  http://localhost:8080/remote.php/dav/files/alice/notes.txt
```

The sink receives
`{"event":"files.uploaded","user":"alice","path":"/notes.txt","size":123}`.

## Caveats

- The guest imports `github.com/vmihailenco/msgpack/v5` (already a host
  dependency). Its TinyGo/`wasm-unknown` compatibility is assumed but
  unverified in this environment — TinyGo is not installed here, so the
  committed sources are validated via the `!tinygo` stub build, `go vet`,
  and `ncgo-cli plugin check` only.
