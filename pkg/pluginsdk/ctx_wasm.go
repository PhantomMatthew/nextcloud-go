//go:build wasm

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

func readHostString(fn func(outPtr, outMax int32) int32, max int32) string {
	ptr := alloc(max)
	n := fn(ptr, max)
	if n <= 0 {
		return ""
	}
	return string(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(uint32(ptr)))), int(n)))
}

// CtxUserID returns the id of the user behind the current request.
func CtxUserID() string { return readHostString(hostCtxUserID, 256) }

// CtxRequestID returns the current request id.
func CtxRequestID() string { return readHostString(hostCtxRequestID, 256) }

// CtxLocale returns the current request locale.
func CtxLocale() string { return readHostString(hostCtxLocale, 64) }

// CtxDeadlineUnixMS returns the call deadline in unix ms, or 0 if none.
func CtxDeadlineUnixMS() int64 { return hostCtxDeadlineUnixMS() }
