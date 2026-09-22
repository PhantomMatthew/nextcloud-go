package plugins

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// cacheKeyPrefix namespaces all of plugin id's cache keys under plugin:<id>:.
func cacheKeyPrefix(id string) string {
	return "plugin:" + id + ":"
}

// cacheKey namespaces the plugin-supplied key under plugin:<id>:.
func cacheKey(ctx context.Context, key string) string {
	info := callFromCtx(ctx)
	id := "unknown"
	if info.plugin != nil {
		id = info.plugin.manifest.Plugin.ID
	}
	return cacheKeyPrefix(id) + key
}

func (h *Host) cacheErrUnavailable() int32 {
	return pluginsdk.ErrCodeUnavailable
}

// cacheGet reads a namespaced key into the guest buffer. Always granted;
// availability depends on a configured host cache.
func (h *Host) cacheGet(ctx context.Context, mod api.Module, keyPtr, keyLen, outPtr, outMax int32) int32 {
	if h.cfg.Cache == nil {
		return h.cacheErrUnavailable()
	}
	key, code := readString(mod, keyPtr, keyLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	val, err := h.cfg.Cache.Get(ctx, cacheKey(ctx, key))
	if errors.Is(err, cache.ErrMiss) {
		return pluginsdk.ErrCodeNotFound
	}
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return writeBytes(mod, outPtr, outMax, val)
}

// cacheSet stores val under a namespaced key with a TTL in seconds
// (0 = no expiry).
func (h *Host) cacheSet(ctx context.Context, mod api.Module, keyPtr, keyLen, valPtr, valLen, ttlSeconds int32) int32 {
	if h.cfg.Cache == nil {
		return h.cacheErrUnavailable()
	}
	if ttlSeconds < 0 {
		return pluginsdk.ErrCodeInvalidArgument
	}
	key, code := readString(mod, keyPtr, keyLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	val, code := readBytes(mod, valPtr, valLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if err := h.cfg.Cache.Set(ctx, cacheKey(ctx, key), val, time.Duration(ttlSeconds)*time.Second); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}

// cacheDelete removes a namespaced key; deleting a missing key is OK.
func (h *Host) cacheDelete(ctx context.Context, mod api.Module, keyPtr, keyLen int32) int32 {
	if h.cfg.Cache == nil {
		return h.cacheErrUnavailable()
	}
	key, code := readString(mod, keyPtr, keyLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if err := h.cfg.Cache.Delete(ctx, cacheKey(ctx, key)); err != nil && !errors.Is(err, cache.ErrMiss) {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}

// cacheIncrement applies delta to a namespaced counter and writes the new
// value as 8 little-endian bytes at out_new (out_new == 0 skips the write).
func (h *Host) cacheIncrement(ctx context.Context, mod api.Module, keyPtr, keyLen int32, delta int64, outNew int32) int32 {
	if h.cfg.Cache == nil {
		return h.cacheErrUnavailable()
	}
	key, code := readString(mod, keyPtr, keyLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	newVal, err := h.cfg.Cache.Increment(ctx, cacheKey(ctx, key), delta)
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	if outNew == 0 {
		return pluginsdk.ErrCodeOK
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(newVal)) //nolint:gosec // G115: intentional bit-cast of counter value
	return writeBytes(mod, outNew, 8, buf[:])
}
