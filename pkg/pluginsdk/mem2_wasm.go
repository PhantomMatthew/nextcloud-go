//go:build wasm

package pluginsdk

import "unsafe"

func writeByte(ptr int32, b byte) {
	*(*byte)(unsafe.Pointer(uintptr(uint32(ptr)))) = b
}

func copyTo(ptr int32, data []byte) {
	dst := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(uint32(ptr)))), len(data))
	copy(dst, data)
}

// readI64LE reads 8 little-endian bytes from linear memory.
func readI64LE(ptr int32) int64 {
	b := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(uint32(ptr)))), 8)
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return int64(v)
}

// assignValue assigns a MessagePack-decoded value to a Scan destination.
func assignValue(dest, val any) int32 {
	switch d := dest.(type) {
	case *string:
		switch v := val.(type) {
		case string:
			*d = v
		case []byte:
			*d = string(v)
		default:
			return ErrCodeInvalidArgument
		}
	case *int64:
		switch v := val.(type) {
		case int64:
			*d = v
		case uint64:
			if v > 1<<63-1 {
				return ErrCodeInvalidArgument
			}
			*d = int64(v)
		default:
			return ErrCodeInvalidArgument
		}
	case *float64:
		switch v := val.(type) {
		case float64:
			*d = v
		case float32:
			*d = float64(v)
		case int64:
			*d = float64(v)
		default:
			return ErrCodeInvalidArgument
		}
	case *bool:
		v, ok := val.(bool)
		if !ok {
			return ErrCodeInvalidArgument
		}
		*d = v
	case *[]byte:
		switch v := val.(type) {
		case []byte:
			*d = v
		case string:
			*d = []byte(v)
		default:
			return ErrCodeInvalidArgument
		}
	case *any:
		*d = val
	default:
		return ErrCodeInvalidArgument
	}
	return ErrCodeOK
}
