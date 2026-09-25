//go:build !tinygo

package pluginsdk

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

// HTTPRequestArgs is a no-op on non-wasm builds.
func HTTPRequestArgs(_, _ int32) []byte { return nil }

// DecodeHTTPRequest is a no-op on non-wasm builds.
func DecodeHTTPRequest(_, _ int32) (*HTTPRequest, int32) { return nil, ErrCodeUnsupported }
