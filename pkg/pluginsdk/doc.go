// Package pluginsdk is the stable guest-side SDK for building WASM plugins
// targeting the ncgo-abi/1 contract. See docs/specs/wasm-plugin-abi.md.
//
// Plugins compile with TinyGo for wasip1 reactor mode (ADR-0092):
//
//	tinygo build -o plugin.wasm -target=wasip1 -buildmode=c-shared -no-debug .
//
// The real bindings live in the `//go:build tinygo` files; host-side Go code
// importing this package gets the no-op stubs (`!tinygo`). The `wasm` build
// tag and `_wasm.go` filename suffix must not be used for this split:
// TinyGo's wasm-unknown target never sets them (its GOARCH is arm).
package pluginsdk
