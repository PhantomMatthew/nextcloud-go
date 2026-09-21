package plugins

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"hash"

	"github.com/tetratelabs/wazero/api"
	"golang.org/x/crypto/blake2b"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// maxRandomBytes caps one crypto_random call.
const maxRandomBytes = 64 * 1024

func hashFor(algo int32) (func() hash.Hash, int32) {
	switch algo {
	case pluginsdk.HashSHA256:
		return sha256.New, pluginsdk.ErrCodeOK
	case pluginsdk.HashSHA512:
		return sha512.New, pluginsdk.ErrCodeOK
	case pluginsdk.HashBLAKE2b:
		return func() hash.Hash {
			h, err := blake2b.New256(nil)
			if err != nil { // never fails for nil key
				panic(err)
			}
			return h
		}, pluginsdk.ErrCodeOK
	default:
		return nil, pluginsdk.ErrCodeUnsupported
	}
}

// cryptoRandom fills out_ptr with out_len random bytes; always granted.
func (h *Host) cryptoRandom(_ context.Context, mod api.Module, outPtr, outLen int32) int32 {
	if outPtr < 0 || outLen < 0 {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if outLen > maxRandomBytes {
		return pluginsdk.ErrCodeTooLarge
	}
	buf := make([]byte, outLen)
	if _, err := rand.Read(buf); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return writeBytes(mod, outPtr, outLen, buf)
}

// cryptoHash writes the digest of the input; always granted.
func (h *Host) cryptoHash(_ context.Context, mod api.Module, algo, inPtr, inLen, outPtr, outMax int32) int32 {
	newHash, code := hashFor(algo)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	in, code := readBytes(mod, inPtr, inLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	hh := newHash()
	_, _ = hh.Write(in)
	return writeBytes(mod, outPtr, outMax, hh.Sum(nil))
}

// cryptoHMAC writes the HMAC of msg under key; always granted.
func (h *Host) cryptoHMAC(_ context.Context, mod api.Module, algo, keyPtr, keyLen, msgPtr, msgLen, outPtr, outMax int32) int32 {
	newHash, code := hashFor(algo)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	key, code := readBytes(mod, keyPtr, keyLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	msg, code := readBytes(mod, msgPtr, msgLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	mac := hmac.New(newHash, key)
	_, _ = mac.Write(msg)
	return writeBytes(mod, outPtr, outMax, mac.Sum(nil))
}
