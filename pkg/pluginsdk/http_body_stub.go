//go:build !tinygo

package pluginsdk

// HTTPBodyCreate is a no-op on non-wasm builds.
func HTTPBodyCreate() (int32, int32) { return 0, ErrCodeUnsupported }

// HTTPBodyWrite is a no-op on non-wasm builds.
func HTTPBodyWrite(_ int32, _ []byte) (int, int32) { return 0, ErrCodeUnsupported }

// HTTPBodyClose is a no-op on non-wasm builds.
func HTTPBodyClose(_ int32) int32 { return ErrCodeUnsupported }
