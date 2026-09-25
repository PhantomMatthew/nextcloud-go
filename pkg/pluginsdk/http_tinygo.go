//go:build tinygo

package pluginsdk

import (
	"github.com/vmihailenco/msgpack/v5"
)

// HTTPRequest is the MessagePack request map passed to an on_request entry
// point. The body travels inline as BodyBytes, or — for plugins with
// runtime.request_body_stream — as BodyHandle, pulled via RequestBodyRead
// (spec §7); exactly one of the two carries the body.
type HTTPRequest struct {
	Method     string            `msgpack:"method"`
	Path       string            `msgpack:"path"`
	Query      string            `msgpack:"query"`
	Headers    map[string]string `msgpack:"headers"`
	BodyBytes  []byte            `msgpack:"body_bytes"`
	BodyHandle int32             `msgpack:"body_handle"`
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
