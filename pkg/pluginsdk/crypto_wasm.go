//go:build wasm

package pluginsdk

import "unsafe"

//go:wasmimport ncgo crypto_random
func hostCryptoRandom(outPtr, outLen int32) int32

//go:wasmimport ncgo crypto_hash
func hostCryptoHash(algo, inPtr, inLen, outPtr, outMax int32) int32

//go:wasmimport ncgo crypto_hmac
func hostCryptoHMAC(algo, keyPtr, keyLen, msgPtr, msgLen, outPtr, outMax int32) int32

func bytesPtr(b []byte) (int32, int32) {
	if len(b) == 0 {
		return 0, 0
	}
	return int32(uintptr(unsafe.Pointer(unsafe.SliceData(b)))), int32(len(b))
}

func readAt(ptr, n int32) []byte {
	if n <= 0 {
		return nil
	}
	out := make([]byte, n)
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(uintptr(uint32(ptr)))), int(n)))
	return out
}

// CryptoRandom returns n random bytes from the host.
func CryptoRandom(n int32) ([]byte, int32) {
	if n <= 0 {
		return nil, ErrCodeInvalidArgument
	}
	ptr := alloc(n)
	if code := hostCryptoRandom(ptr, n); code < 0 {
		return nil, code
	}
	return readAt(ptr, n), ErrCodeOK
}

// CryptoHash hashes data with algo (HashSHA256, HashSHA512, HashBLAKE2b).
func CryptoHash(algo int32, data []byte) ([]byte, int32) {
	inPtr, inLen := bytesPtr(data)
	outPtr := alloc(64)
	n := hostCryptoHash(algo, inPtr, inLen, outPtr, 64)
	if n < 0 {
		return nil, n
	}
	return readAt(outPtr, n), ErrCodeOK
}

// CryptoHMAC computes HMAC of msg under key with algo.
func CryptoHMAC(algo int32, key, msg []byte) ([]byte, int32) {
	keyPtr, keyLen := bytesPtr(key)
	msgPtr, msgLen := bytesPtr(msg)
	outPtr := alloc(64)
	n := hostCryptoHMAC(algo, keyPtr, keyLen, msgPtr, msgLen, outPtr, 64)
	if n < 0 {
		return nil, n
	}
	return readAt(outPtr, n), ErrCodeOK
}
