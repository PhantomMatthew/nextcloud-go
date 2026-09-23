package plugins

import (
	"context"
	"errors"
	"io"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// requestBodyHandle wraps an inbound route request body as a stream handle
// (kind handleStream): a request body is a stream, so it shares the spec §8
// 64-stream budget with storage streams. Close is a no-op — req.Body belongs
// to net/http, which closes it after the handler returns; the handle only
// gates guest visibility and must not double-close the body.
type requestBodyHandle struct {
	body io.Reader
}

func (r *requestBodyHandle) Read(p []byte) (int, error) { return r.body.Read(p) }

func (r *requestBodyHandle) Close() error { return nil }

// requestBodyFromHandle resolves a request body handle: a missing id is
// ErrCodeNotFound, an id of any other handle type (storage stream, rows,
// outbound HTTP response) is ErrCodeInvalidArgument.
func (h *Host) requestBodyFromHandle(mod api.Module, handle int32) (*requestBodyHandle, int32) {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	e, ok := tabs.getAny(handle)
	if !ok {
		return nil, pluginsdk.ErrCodeNotFound
	}
	rb, ok := e.val.(*requestBodyHandle)
	if !ok {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	return rb, pluginsdk.ErrCodeOK
}

// requestBodyRead mirrors http_response_body_read for the inbound direction
// (spec §7 body_handle): bytes read (>0), 0 at EOF, negative error code. The
// body is the plugin's own inbound request, so no capability gates it.
func (h *Host) requestBodyRead(_ context.Context, mod api.Module, handle, bufPtr, bufMax int32) int32 {
	rb, code := h.requestBodyFromHandle(mod, handle)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if bufMax < 0 || bufMax > maxPayloadArg {
		return pluginsdk.ErrCodeTooLarge
	}
	if bufMax == 0 {
		return pluginsdk.ErrCodeOK
	}
	buf := make([]byte, bufMax)
	n, err := rb.Read(buf)
	if n > 0 {
		return writeBytes(mod, bufPtr, bufMax, buf[:n])
	}
	if errors.Is(err, io.EOF) {
		return pluginsdk.ErrCodeOK // EOF
	}
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}

// requestBodyClose removes the handle from the table; later reads answer
// ErrCodeNotFound. The body itself stays owned by net/http (see
// requestBodyHandle).
func (h *Host) requestBodyClose(_ context.Context, mod api.Module, handle int32) int32 {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	e, ok := tabs.getAny(handle)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	if _, ok := e.val.(*requestBodyHandle); !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	tabs.remove(handle, handleStream)
	return pluginsdk.ErrCodeOK
}
