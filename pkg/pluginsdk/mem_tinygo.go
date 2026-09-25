//go:build tinygo

package pluginsdk

import "unsafe"

var bump uint32 = 1024

//go:wasmexport ncgo_abi_version
func abiVersion() int32 { return ABIVersion }

//go:wasmexport ncgo_alloc
func alloc(size int32) int32 {
	if size <= 0 {
		return 0
	}
	ptr := bump
	bump = (ptr + uint32(size) + 7) &^ 7
	return int32(ptr)
}

//go:wasmexport ncgo_free
func free(_, _ int32) {}

func allocString(s string) int32 {
	n := int32(len(s))
	ptr := alloc(n)
	if n == 0 || ptr == 0 {
		return ptr
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(uint32(ptr)))), len(s))
	copy(dst, s)
	return ptr
}
