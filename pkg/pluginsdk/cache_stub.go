//go:build !wasm

package pluginsdk

// CacheGet is a no-op on non-wasm builds so the host can import this package.
func CacheGet(string) ([]byte, int32) { return nil, ErrCodeUnsupported }

// CacheSet is a no-op on non-wasm builds.
func CacheSet(string, []byte, int32) int32 { return ErrCodeUnsupported }

// CacheDelete is a no-op on non-wasm builds.
func CacheDelete(string) int32 { return ErrCodeUnsupported }

// CacheIncrement is a no-op on non-wasm builds.
func CacheIncrement(string, int64) (int64, int32) { return 0, ErrCodeUnsupported }
