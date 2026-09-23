//go:build wasm

package pluginsdk

//go:wasmimport ncgo http_request_body_create
func hostHTTPBodyCreate() int64

//go:wasmimport ncgo http_request_body_write
func hostHTTPBodyWrite(handle, bufPtr, bufLen int32) int32

//go:wasmimport ncgo http_request_body_close
func hostHTTPBodyClose(handle int32) int32

// HTTPBodyCreate opens a host-side spool for a streamed outbound request
// body (spec §6.3): write it in chunks with HTTPBodyWrite, seal it with
// HTTPBodyClose, then reference it from HTTPOutboundRequest.BodyHandle.
// Requires the http.outbound capability.
func HTTPBodyCreate() (int32, int32) {
	return unpackI64(hostHTTPBodyCreate())
}

// HTTPBodyWrite appends buf to the spool and returns the byte count. The
// running total may not exceed the host spool cap (default 1 GiB, the same
// knob as storage write spools); the crossing write fails with
// ErrCodeTooLarge. Writing a sealed spool fails with ErrCodeInvalidArgument.
func HTTPBodyWrite(handle int32, buf []byte) (int, int32) {
	if len(buf) == 0 {
		return 0, ErrCodeOK
	}
	ptr := alloc(int32(len(buf)))
	copyTo(ptr, buf)
	n := hostHTTPBodyWrite(handle, ptr, int32(len(buf)))
	if n < 0 {
		return 0, n
	}
	return int(n), ErrCodeOK
}

// HTTPBodyClose seals the spool; only a sealed handle is accepted by
// http_request, which consumes and destroys the spool on use. Writes and
// repeated closes after the seal fail with ErrCodeInvalidArgument. A handle
// never consumed is discarded by instance cleanup.
func HTTPBodyClose(handle int32) int32 { return hostHTTPBodyClose(handle) }
