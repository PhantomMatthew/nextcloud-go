package plugins

import (
	"context"
	"log/slog"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

func (h *Host) log(ctx context.Context, mod api.Module, level, ptr, length int32) int32 {
	if level < 0 || level > 3 || ptr < 0 || length < 0 {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if length > 65536 {
		return pluginsdk.ErrCodeTooLarge
	}
	mem := mod.Memory()
	if mem == nil {
		return pluginsdk.ErrCodeInternal
	}
	msg, ok := mem.Read(uint32(ptr), uint32(length))
	if !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if h.logger == nil {
		return pluginsdk.ErrCodeOK
	}
	id, ok := ctx.Value(ctxPluginID).(string)
	if !ok || id == "" {
		id = mod.Name()
	}
	ver, ok := ctx.Value(ctxPluginVersion).(string)
	if !ok {
		ver = ""
	}
	switch level {
	case pluginsdk.LevelDebug:
		h.logger.DebugContext(ctx, string(msg), slog.String("plugin.id", id), slog.String("plugin.version", ver))
	case pluginsdk.LevelInfo:
		h.logger.InfoContext(ctx, string(msg), slog.String("plugin.id", id), slog.String("plugin.version", ver))
	case pluginsdk.LevelWarn:
		h.logger.WarnContext(ctx, string(msg), slog.String("plugin.id", id), slog.String("plugin.version", ver))
	default:
		h.logger.ErrorContext(ctx, string(msg), slog.String("plugin.id", id), slog.String("plugin.version", ver))
	}
	return pluginsdk.ErrCodeOK
}
