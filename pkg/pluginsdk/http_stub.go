//go:build !wasm

package pluginsdk

// HTTPRequest is the MessagePack request map passed to an on_request entry
// point.
type HTTPRequest struct {
	Method    string            `msgpack:"method"`
	Path      string            `msgpack:"path"`
	Query     string            `msgpack:"query"`
	Headers   map[string]string `msgpack:"headers"`
	BodyBytes []byte            `msgpack:"body_bytes"`
}

// HTTPRequestArgs is a no-op on non-wasm builds.
func HTTPRequestArgs(_, _ int32) []byte { return nil }

// DecodeHTTPRequest is a no-op on non-wasm builds.
func DecodeHTTPRequest(_, _ int32) (*HTTPRequest, int32) { return nil, ErrCodeUnsupported }
