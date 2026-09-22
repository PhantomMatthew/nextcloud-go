//go:build wasm

package pluginsdk

import (
	"github.com/vmihailenco/msgpack/v5"
)

// HTTPRequest is the MessagePack request map passed to an on_request entry
// point. The body travels inline as body_bytes (spec §7 deviation: no
// body_handle in ABI v1).
type HTTPRequest struct {
	Method    string            `msgpack:"method"`
	Path      string            `msgpack:"path"`
	Query     string            `msgpack:"query"`
	Headers   map[string]string `msgpack:"headers"`
	BodyBytes []byte            `msgpack:"body_bytes"`
}

// HTTPRequestArgs reads the raw MessagePack request passed to an on_request
// entry point.
func HTTPRequestArgs(reqPtr, reqLen int32) []byte {
	return readAt(reqPtr, reqLen)
}

// DecodeHTTPRequest reads and decodes the request passed to an on_request
// entry point.
func DecodeHTTPRequest(reqPtr, reqLen int32) (*HTTPRequest, int32) {
	var req HTTPRequest
	if err := msgpack.Unmarshal(readAt(reqPtr, reqLen), &req); err != nil {
		return nil, ErrCodeInvalidArgument
	}
	return &req, ErrCodeOK
}
