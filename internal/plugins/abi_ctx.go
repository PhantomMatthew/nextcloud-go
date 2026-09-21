package plugins

import (
	"context"

	"github.com/tetratelabs/wazero/api"
)

// ctxUserID writes the calling request's user id; always granted.
func (h *Host) ctxUserID(ctx context.Context, mod api.Module, outPtr, outMax int32) int32 {
	return writeBytes(mod, outPtr, outMax, []byte(callFromCtx(ctx).call.UserID))
}

// ctxRequestID writes the calling request's id; always granted.
func (h *Host) ctxRequestID(ctx context.Context, mod api.Module, outPtr, outMax int32) int32 {
	return writeBytes(mod, outPtr, outMax, []byte(callFromCtx(ctx).call.RequestID))
}

// ctxLocale writes the calling request's locale; always granted.
func (h *Host) ctxLocale(ctx context.Context, mod api.Module, outPtr, outMax int32) int32 {
	return writeBytes(mod, outPtr, outMax, []byte(callFromCtx(ctx).call.Locale))
}

// ctxDeadlineUnixMS returns the call deadline in unix milliseconds, or 0.
func (h *Host) ctxDeadlineUnixMS(ctx context.Context, _ api.Module) int64 {
	dl := callFromCtx(ctx).call.Deadline
	if dl.IsZero() {
		return 0
	}
	return dl.UnixMilli()
}
