//go:build !wasm

package pluginsdk

// CryptoRandom is a no-op on non-wasm builds so the host can import this package.
func CryptoRandom(int32) ([]byte, int32) { return nil, ErrCodeUnsupported }

// CryptoHash is a no-op on non-wasm builds.
func CryptoHash(int32, []byte) ([]byte, int32) { return nil, ErrCodeUnsupported }

// CryptoHMAC is a no-op on non-wasm builds.
func CryptoHMAC(int32, []byte, []byte) ([]byte, int32) { return nil, ErrCodeUnsupported }
