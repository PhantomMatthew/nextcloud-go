//go:build !tinygo

package pluginsdk

// RequestBodyRead is a no-op on non-wasm builds.
func RequestBodyRead(_ int32, _ []byte) (int, int32) { return 0, ErrCodeUnsupported }

// RequestBodyClose is a no-op on non-wasm builds.
func RequestBodyClose(_ int32) int32 { return ErrCodeUnsupported }
