//go:build tinygo

package pluginsdk

//go:wasmimport ncgo request_body_read
func hostRequestBodyRead(handle, bufPtr, bufMax int32) int32

//go:wasmimport ncgo request_body_close
func hostRequestBodyClose(handle int32) int32

// RequestBodyRead fills buf from the request body stream behind the
// body_handle of an on_request request map (spec §7); n == 0 with ErrCodeOK
// means EOF. Only plugins with runtime.request_body_stream receive a
// body_handle; others read the inline BodyBytes of HTTPRequest.
func RequestBodyRead(handle int32, buf []byte) (int, int32) {
	if len(buf) == 0 {
		return 0, ErrCodeOK
	}
	ptr := alloc(int32(len(buf)))
	n := hostRequestBodyRead(handle, ptr, int32(len(buf)))
	if n < 0 {
		return 0, n
	}
	copy(buf, readAt(ptr, n))
	return int(n), ErrCodeOK
}

// RequestBodyClose releases a body_handle early; the body itself stays owned
// by the host. Reading a closed handle answers ErrCodeNotFound.
func RequestBodyClose(handle int32) int32 { return hostRequestBodyClose(handle) }
