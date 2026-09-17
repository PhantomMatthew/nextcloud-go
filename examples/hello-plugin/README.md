# Hello plugin

TinyGo guest that logs `hello from wasm` from `ncgo_on_install` via `ncgo.log`.

Build (requires TinyGo with `wasm-unknown`):

```bash
make example-plugin
```

Then check it with the operator CLI:

```bash
go run ./cmd/ncgo-cli plugin check examples/hello-plugin
```
