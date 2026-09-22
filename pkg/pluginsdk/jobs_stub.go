//go:build !wasm

package pluginsdk

// JobEnqueue is a no-op on non-wasm builds.
func JobEnqueue(string, []byte, int64) int32 { return ErrCodeUnsupported }

// JobArgs is a no-op on non-wasm builds.
func JobArgs(_, _, _, _ int32) (string, []byte) { return "", nil }
