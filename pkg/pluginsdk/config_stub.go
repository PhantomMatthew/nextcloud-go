//go:build !tinygo

package pluginsdk

// ConfigMaxValue is the host's per-key config value cap (64KiB).
const ConfigMaxValue = 64 << 10

// ConfigGet is a no-op on non-wasm builds so the host can import this package.
func ConfigGet(string, []byte) int32 { return ErrCodeUnsupported }

// ConfigSet is a no-op on non-wasm builds.
func ConfigSet(string, []byte) int32 { return ErrCodeUnsupported }
