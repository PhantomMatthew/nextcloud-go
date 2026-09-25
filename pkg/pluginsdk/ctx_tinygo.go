//go:build tinygo

package pluginsdk

import "unsafe"

//go:wasmimport ncgo ctx_user_id
func hostCtxUserID(outPtr, outMax int32) int32

//go:wasmimport ncgo ctx_request_id
func hostCtxRequestID(outPtr, outMax int32) int32

//go:wasmimport ncgo ctx_locale
func hostCtxLocale(outPtr, outMax int32) int32

//go:wasmimport ncgo ctx_deadline_unix_ms
func hostCtxDeadlineUnixMS() int64

// readStringAt copies n bytes from guest memory at ptr. TinyGo does not
// allow wasmimport functions as first-class values, so each getter calls
// its import directly instead of sharing a callback-shaped helper.
func readStringAt(ptr, n int32) string {
	if n <= 0 {
		return ""
	}
	return string(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(uint32(ptr)))), int(n)))
}

// CtxUserID returns the id of the user behind the current request.
func CtxUserID() string {
	ptr := alloc(256)
	return readStringAt(ptr, hostCtxUserID(ptr, 256))
}

// CtxRequestID returns the current request id.
func CtxRequestID() string {
	ptr := alloc(256)
	return readStringAt(ptr, hostCtxRequestID(ptr, 256))
}

// CtxLocale returns the current request locale.
func CtxLocale() string {
	ptr := alloc(64)
	return readStringAt(ptr, hostCtxLocale(ptr, 64))
}

// CtxDeadlineUnixMS returns the call deadline in unix ms, or 0 if none.
func CtxDeadlineUnixMS() int64 { return hostCtxDeadlineUnixMS() }
