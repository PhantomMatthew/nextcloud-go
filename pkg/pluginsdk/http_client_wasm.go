//go:build wasm

package pluginsdk

import (
	"github.com/vmihailenco/msgpack/v5"
)

//go:wasmimport ncgo http_request
func hostHTTPRequest(reqPtr, reqLen int32) int64

//go:wasmimport ncgo http_response_status
func hostHTTPResponseStatus(handle int32) int32

//go:wasmimport ncgo http_response_header
func hostHTTPResponseHeader(handle, namePtr, nameLen, outPtr, outMax int32) int32

//go:wasmimport ncgo http_response_body_read
func hostHTTPResponseBodyRead(handle, bufPtr, bufMax int32) int32

//go:wasmimport ncgo http_response_close
func hostHTTPResponseClose(handle int32) int32

// httpOutCap bounds header/body scratch buffers used by the bindings.
const httpOutCap = 1 << 20

// HTTPOutboundRequest is the MessagePack request map passed to http_request.
// TimeoutMS <= 0 selects the host default (10s); values above 30s are
// clamped by the host. The body travels inline as Body or by reference as
// BodyHandle — a sealed spool from HTTPBodyCreate/HTTPBodyClose, consumed
// and destroyed by the call — never both: the host rejects a request map
// carrying a non-empty Body and a non-zero BodyHandle with
// ErrCodeInvalidArgument. BodyHandle is omitempty so requests that do not
// stream keep the 4c5 map shape.
type HTTPOutboundRequest struct {
	Method     string            `msgpack:"method"`
	URL        string            `msgpack:"url"`
	Headers    map[string]string `msgpack:"headers"`
	Body       []byte            `msgpack:"body_bytes"`
	BodyHandle int32             `msgpack:"body_handle,omitempty"`
	TimeoutMS  int32             `msgpack:"timeout_ms"`
}

// HTTPResponse is an open outbound response; Close releases the handle.
type HTTPResponse struct {
	handle int32
}

// HTTPDo issues req and returns the response handle.
func HTTPDo(req *HTTPOutboundRequest) (*HTTPResponse, int32) {
	raw, err := msgpack.Marshal(req)
	if err != nil {
		return nil, ErrCodeInvalidArgument
	}
	ptr := alloc(int32(len(raw)))
	copyTo(ptr, raw)
	errCode, handle := unpackI64(hostHTTPRequest(ptr, int32(len(raw))))
	if errCode != ErrCodeOK {
		return nil, errCode
	}
	return &HTTPResponse{handle: handle}, ErrCodeOK
}

// Status returns the HTTP status code (negative = error code).
func (r *HTTPResponse) Status() int32 {
	return hostHTTPResponseStatus(r.handle)
}

// Header returns the response header values joined with ", "; an absent
// header yields "".
func (r *HTTPResponse) Header(name string) (string, int32) {
	namePtr, nameLen := bytesPtr([]byte(name))
	outPtr := alloc(64 << 10)
	n := hostHTTPResponseHeader(r.handle, namePtr, nameLen, outPtr, 64<<10)
	if n < 0 {
		return "", n
	}
	return string(readAt(outPtr, n)), ErrCodeOK
}

// Read fills buf from the response body; 0 with ErrCodeOK means EOF.
func (r *HTTPResponse) Read(buf []byte) (int, int32) {
	if len(buf) == 0 {
		return 0, ErrCodeOK
	}
	ptr := alloc(int32(len(buf)))
	n := hostHTTPResponseBodyRead(r.handle, ptr, int32(len(buf)))
	if n < 0 {
		return 0, n
	}
	copy(buf, readAt(ptr, n))
	return int(n), ErrCodeOK
}

// Close releases the response handle and body.
func (r *HTTPResponse) Close() int32 { return hostHTTPResponseClose(r.handle) }
