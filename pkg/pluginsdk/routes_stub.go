//go:build !tinygo

package pluginsdk

// RouteRegister is a no-op on non-wasm builds.
func RouteRegister(_, _, _ string) int32 { return ErrCodeUnsupported }

// OCSRegister is a no-op on non-wasm builds.
func OCSRegister(_, _, _ string) int32 { return ErrCodeUnsupported }
