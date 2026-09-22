package plugins

import (
	"context"
	"errors"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/internal/appconfig"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// configAppID is the appconfig appid all plugin config rows live under; the
// stored key is <plugin_id>.<key> (spec §6.3: keys auto-namespaced under
// "plugin.<plugin_id>").
const configAppID = "plugin"

// configKey namespaces the plugin-local key under the calling plugin's id.
func configKey(ctx context.Context, key string) string {
	info := callFromCtx(ctx)
	id := "unknown"
	if info.plugin != nil {
		id = info.plugin.manifest.Plugin.ID
	}
	return id + "." + key
}

// configGet reads a config value into the guest buffer. Capability globs
// match the plugin-local key, not the namespaced storage key. Keys are
// unrestricted byte strings except empty (rejected).
func (h *Host) configGet(ctx context.Context, mod api.Module, keyPtr, keyLen, outPtr, outMax int32) int32 {
	key, code := readString(mod, keyPtr, keyLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if key == "" {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if !pluginCaps(ctx).canConfigRead(key) {
		return pluginsdk.ErrCodePermissionDenied
	}
	if h.cfg.AppConfig == nil {
		return pluginsdk.ErrCodeUnavailable
	}
	val, err := h.cfg.AppConfig.Get(ctx, configAppID, configKey(ctx, key))
	if errors.Is(err, appconfig.ErrNotFound) {
		return pluginsdk.ErrCodeNotFound
	}
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return writeBytes(mod, outPtr, outMax, []byte(val))
}

// configSet stores val under the plugin-local key; values are capped at
// maxStringArg (64KiB).
func (h *Host) configSet(ctx context.Context, mod api.Module, keyPtr, keyLen, valPtr, valLen int32) int32 {
	key, code := readString(mod, keyPtr, keyLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if key == "" {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if !pluginCaps(ctx).canConfigWrite(key) {
		return pluginsdk.ErrCodePermissionDenied
	}
	if valLen > maxStringArg {
		return pluginsdk.ErrCodeTooLarge
	}
	if h.cfg.AppConfig == nil {
		return pluginsdk.ErrCodeUnavailable
	}
	val, code := readBytes(mod, valPtr, valLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if err := h.cfg.AppConfig.Set(ctx, configAppID, configKey(ctx, key), string(val)); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}
