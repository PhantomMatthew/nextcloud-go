package plugins

import (
	"context"
	"log/slog"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// maxStringArg bounds length-prefixed string/key arguments; maxPayloadArg
// bounds opaque value/payload arguments.
const (
	maxStringArg  = 64 * 1024
	maxPayloadArg = 1 << 20
)

// registerHostModule exposes the full ncgo-abi/1 host function surface so
// guest modules linking any of them can instantiate. Functions backed by
// subsystems not yet implemented check capabilities and return
// ErrUnsupported.
func (h *Host) registerHostModule(ctx context.Context) error {
	b := h.rt.NewHostModuleBuilder("ncgo")
	export := func(name string, fn any) {
		b.NewFunctionBuilder().WithFunc(h.wrapHostMetrics(name, fn)).Export(name)
	}
	export("log", h.log)

	export("ctx_user_id", h.ctxUserID)
	export("ctx_request_id", h.ctxRequestID)
	export("ctx_locale", h.ctxLocale)
	export("ctx_deadline_unix_ms", h.ctxDeadlineUnixMS)

	export("config_get", h.configGet)
	export("config_set", h.configSet)

	export("db_query", h.dbQuery)
	export("db_exec", h.dbExec)
	export("db_rows_next", h.dbRowsNext)
	export("db_rows_close", h.dbRowsClose)
	export("db_tx_begin", h.dbTxBegin)
	export("db_tx_commit", h.dbTxCommit)
	export("db_tx_rollback", h.dbTxRollback)
	export("db_tx_query", h.dbTxQuery)
	export("db_tx_exec", h.dbTxExec)

	export("cache_get", h.cacheGet)
	export("cache_set", h.cacheSet)
	export("cache_delete", h.cacheDelete)
	export("cache_increment", h.cacheIncrement)

	export("storage_stat", h.storageStat)
	export("storage_open", h.storageOpen)
	export("storage_create", h.storageCreate)
	export("storage_stream_read", h.storageStreamRead)
	export("storage_stream_write", h.storageStreamWrite)
	export("storage_stream_close", h.storageStreamClose)
	export("storage_delete", h.storageDelete)
	export("storage_list", h.storageList)
	export("storage_rename", h.storageRename)

	export("http_request", h.httpRequest)
	export("http_response_status", h.httpResponseStatus)
	export("http_response_header", h.httpResponseHeader)
	export("http_response_body_read", h.httpResponseBodyRead)
	export("http_response_close", h.httpResponseClose)
	export("http_request_body_create", h.httpRequestBodyCreate)
	export("http_request_body_write", h.httpRequestBodyWrite)
	export("http_request_body_close", h.httpRequestBodyClose)

	export("request_body_read", h.requestBodyRead)
	export("request_body_close", h.requestBodyClose)

	export("event_publish", h.eventPublish)
	export("job_enqueue", h.jobEnqueue)
	export("route_register", h.routeRegister)
	export("ocs_register", h.ocsRegister)
	export("webdav_register_prop", h.webdavRegisterProp)

	export("crypto_random", h.cryptoRandom)
	export("crypto_hash", h.cryptoHash)
	export("crypto_hmac", h.cryptoHMAC)

	_, err := b.Instantiate(ctx)
	return err
}

func (h *Host) log(ctx context.Context, mod api.Module, level, ptr, length int32) int32 {
	if level < 0 || level > 3 || ptr < 0 || length < 0 {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if length > maxStringArg {
		return pluginsdk.ErrCodeTooLarge
	}
	msg, code := readBytes(mod, ptr, length)
	if code != pluginsdk.ErrCodeOK {
		return code
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

// readBytes copies length bytes from the guest's linear memory.
func readBytes(mod api.Module, ptr, length int32) ([]byte, int32) {
	if ptr < 0 || length < 0 {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	if length > maxPayloadArg {
		return nil, pluginsdk.ErrCodeTooLarge
	}
	mem := mod.Memory()
	if mem == nil {
		return nil, pluginsdk.ErrCodeInternal
	}
	if length == 0 {
		return nil, pluginsdk.ErrCodeOK
	}
	b, ok := mem.Read(uint32(ptr), uint32(length))
	if !ok {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	return b, pluginsdk.ErrCodeOK
}

// readString is readBytes with the tighter string cap.
func readString(mod api.Module, ptr, length int32) (string, int32) {
	if length > maxStringArg {
		return "", pluginsdk.ErrCodeTooLarge
	}
	b, code := readBytes(mod, ptr, length)
	if code != pluginsdk.ErrCodeOK {
		return "", code
	}
	return string(b), pluginsdk.ErrCodeOK
}

// writeBytes copies data into the guest's linear memory, returning the byte
// count or a negative error code. Buffers too small for data are rejected
// with ErrTooLarge; nothing is written.
func writeBytes(mod api.Module, ptr, maxLen int32, data []byte) int32 {
	if ptr < 0 || maxLen < 0 {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if len(data) > int(maxLen) {
		return pluginsdk.ErrCodeTooLarge
	}
	if len(data) == 0 {
		return pluginsdk.ErrCodeOK
	}
	mem := mod.Memory()
	if mem == nil {
		return pluginsdk.ErrCodeInternal
	}
	if !mem.Write(uint32(ptr), data) {
		return pluginsdk.ErrCodeInvalidArgument
	}
	return int32(len(data)) //nolint:gosec // G115: bounded by maxPayloadArg
}

// pluginCaps returns the calling plugin's capabilities; a nil capabilities
// pointer denies everything.
func pluginCaps(ctx context.Context) *Capabilities {
	info := callFromCtx(ctx)
	if info.plugin == nil {
		return nil
	}
	return &info.plugin.manifest.Capabilities
}
