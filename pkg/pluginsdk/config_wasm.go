//go:build wasm

package pluginsdk

//go:wasmimport ncgo config_get
func hostConfigGet(keyPtr, keyLen, outPtr, outMax int32) int32

//go:wasmimport ncgo config_set
func hostConfigSet(keyPtr, keyLen, valPtr, valLen int32) int32

// ConfigMaxValue is the host's per-key config value cap (64KiB).
const ConfigMaxValue = 64 << 10

// ConfigGet reads the plugin-namespaced config key into out, returning the
// byte count or a negative error code: ErrCodeNotFound on a miss,
// ErrCodeTooLarge when out is too small for the value.
func ConfigGet(key string, out []byte) int32 {
	keyPtr := allocString(key)
	if len(out) == 0 {
		return hostConfigGet(keyPtr, int32(len(key)), 0, 0)
	}
	outPtr := alloc(int32(len(out)))
	n := hostConfigGet(keyPtr, int32(len(key)), outPtr, int32(len(out)))
	if n < 0 {
		return n
	}
	copy(out, readAt(outPtr, n))
	return n
}

// ConfigSet stores val under the plugin-namespaced key; val must be at most
// ConfigMaxValue bytes.
func ConfigSet(key string, val []byte) int32 {
	keyPtr := allocString(key)
	valPtr, valLen := bytesPtr(val)
	return hostConfigSet(keyPtr, int32(len(key)), valPtr, valLen)
}
