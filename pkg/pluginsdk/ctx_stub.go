//go:build !tinygo

package pluginsdk

// CtxUserID is a no-op on non-wasm builds so the host can import this package.
func CtxUserID() string { return "" }

// CtxRequestID is a no-op on non-wasm builds.
func CtxRequestID() string { return "" }

// CtxLocale is a no-op on non-wasm builds.
func CtxLocale() string { return "" }

// CtxDeadlineUnixMS is a no-op on non-wasm builds.
func CtxDeadlineUnixMS() int64 { return 0 }
