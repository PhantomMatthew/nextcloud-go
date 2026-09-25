//go:build !tinygo

package pluginsdk

// Debug is a no-op on non-wasm builds so the host module can import this package.
func Debug(string) {}

// Info is a no-op on non-wasm builds so the host module can import this package.
func Info(string) {}

// Warn is a no-op on non-wasm builds so the host module can import this package.
func Warn(string) {}

// Error is a no-op on non-wasm builds so the host module can import this package.
func Error(string) {}
