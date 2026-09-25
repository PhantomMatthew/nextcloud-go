//go:build !tinygo

package pluginsdk

// EventPublish is a no-op on non-wasm builds.
func EventPublish(string, []byte) int32 { return ErrCodeUnsupported }

// EventArgs is a no-op on non-wasm builds.
func EventArgs(_, _, _, _ int32) (string, []byte) { return "", nil }
