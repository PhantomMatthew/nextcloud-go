//go:build tinygo

package pluginsdk

//go:wasmimport ncgo cache_get
func hostCacheGet(keyPtr, keyLen, outPtr, outMax int32) int32

//go:wasmimport ncgo cache_set
func hostCacheSet(keyPtr, keyLen, valPtr, valLen, ttlSeconds int32) int32

//go:wasmimport ncgo cache_delete
func hostCacheDelete(keyPtr, keyLen int32) int32

//go:wasmimport ncgo cache_increment
func hostCacheIncrement(keyPtr, keyLen int32, delta int64, outNew int32) int32

const cacheMaxValue = 1 << 20

// CacheGet reads the plugin-namespaced cache key. ErrCodeNotFound on miss.
func CacheGet(key string) ([]byte, int32) {
	keyPtr := allocString(key)
	outPtr := alloc(cacheMaxValue)
	n := hostCacheGet(keyPtr, int32(len(key)), outPtr, cacheMaxValue)
	if n < 0 {
		return nil, n
	}
	return readAt(outPtr, n), ErrCodeOK
}

// CacheSet stores val under the plugin-namespaced key with a TTL in seconds
// (0 = no expiry).
func CacheSet(key string, val []byte, ttlSeconds int32) int32 {
	keyPtr := allocString(key)
	valPtr, valLen := bytesPtr(val)
	return hostCacheSet(keyPtr, int32(len(key)), valPtr, valLen, ttlSeconds)
}

// CacheDelete removes the plugin-namespaced key.
func CacheDelete(key string) int32 {
	keyPtr := allocString(key)
	return hostCacheDelete(keyPtr, int32(len(key)))
}

// CacheIncrement applies delta to the plugin-namespaced counter and returns
// the new value.
func CacheIncrement(key string, delta int64) (int64, int32) {
	keyPtr := allocString(key)
	outPtr := alloc(8)
	if code := hostCacheIncrement(keyPtr, int32(len(key)), delta, outPtr); code != 0 {
		return 0, code
	}
	b := readAt(outPtr, 8)
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return int64(v), ErrCodeOK
}
