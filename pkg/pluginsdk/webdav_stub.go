//go:build !wasm

package pluginsdk

// WebDAVRegisterProp is a no-op on non-wasm builds.
func WebDAVRegisterProp(_, _, _ string) int32 { return ErrCodeUnsupported }

// WebDAVPropArgs is a no-op on non-wasm builds.
func WebDAVPropArgs(_, _, _, _ int32) (string, []byte) { return "", nil }
