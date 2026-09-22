//go:build !wasm

package pluginsdk

// HTTPOutboundRequest is the MessagePack request map passed to http_request.
// TimeoutMS <= 0 selects the host default (10s); values above 30s are
// clamped by the host.
type HTTPOutboundRequest struct {
	Method    string            `msgpack:"method"`
	URL       string            `msgpack:"url"`
	Headers   map[string]string `msgpack:"headers"`
	Body      []byte            `msgpack:"body_bytes"`
	TimeoutMS int32             `msgpack:"timeout_ms"`
}

// HTTPResponse is a no-op on non-wasm builds so the host can import this
// package.
type HTTPResponse struct{}

// HTTPDo is a no-op on non-wasm builds.
func HTTPDo(*HTTPOutboundRequest) (*HTTPResponse, int32) { return nil, ErrCodeUnsupported }

// Status is a no-op on non-wasm builds.
func (*HTTPResponse) Status() int32 { return ErrCodeUnsupported }

// Header is a no-op on non-wasm builds.
func (*HTTPResponse) Header(string) (string, int32) { return "", ErrCodeUnsupported }

// Read is a no-op on non-wasm builds.
func (*HTTPResponse) Read([]byte) (int, int32) { return 0, ErrCodeUnsupported }

// Close is a no-op on non-wasm builds.
func (*HTTPResponse) Close() int32 { return ErrCodeUnsupported }
