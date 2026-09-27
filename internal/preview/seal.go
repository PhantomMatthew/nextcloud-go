package preview

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

// NCGOPV1 self-sealing of preview blobs under the source file's key
// (ADR-0105 §2): a preview of a v3-sealed file is a thumbnail of its
// plaintext, so it inherits the source file key's confidentiality exactly —
// sealing under any server-held key would open a content side-channel for
// enrolled users. The blob seals single-shot (previews are small):
//
//	NK_pv = HKDF-SHA256(FK, salt="", info="NCGOPV1" || keyUUID)
//	blob  = "NCGOPV1" || keyUUID(16) || nonce(12) ||
//	        AES-256-GCM(NK_pv, nonce, rendered, ad="NCGOPV1" || keyUUID || cacheKey)
//
// The cacheKey in the AD binds the blob to its exact variant (source etag,
// box, fill): a variant-swap replay fails authentication. Blobs live under
// the previews_enc sibling prefix on the RAW backend, so the encrypt
// decorator never sees them and an old binary treats them as a clean cache
// miss (rollback = regenerate into the legacy path).
const (
	// sealMagic marks NCGOPV1 blobs and prefixes both the HKDF info and the
	// GCM associated data.
	sealMagic = "NCGOPV1"

	sealKeyUUIDSize = 16
	sealNonceSize   = 12
	sealTagSize     = 16

	// SealOverhead is the stored-size surplus of an NCGOPV1 blob over its
	// plaintext (magic + key UUID + nonce + GCM tag), so size accounting can
	// report plaintext sizes without opening the blob.
	SealOverhead = len(sealMagic) + sealKeyUUIDSize + sealNonceSize + sealTagSize

	// maxSealedBlob caps the ciphertext size a read will touch: anything
	// larger is rejected before decrypting (ADR-0105 §2).
	maxSealedBlob = 32 << 20
)

// sealPreview renders blob into its NCGOPV1 envelope under the source file
// key fk (named by keyUUID), bound to the exact cacheKey variant.
func sealPreview(fk []byte, keyUUID [sealKeyUUIDSize]byte, cacheKey string, blob []byte) ([]byte, error) {
	aead, err := sealAEAD(sealKey(fk, keyUUID))
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, sealNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("preview: generate seal nonce: %w", err)
	}
	out := make([]byte, 0, SealOverhead+len(blob))
	out = append(out, sealMagic...)
	out = append(out, keyUUID[:]...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, blob, sealAD(keyUUID, cacheKey)), nil
}

// sealKeyUUID parses the NCGOPV1 header, reporting the key UUID and the
// nonce/ciphertext spans; ok is false when the blob does not carry the magic
// (a legacy or foreign blob — never an integrity failure).
func sealKeyUUID(blob []byte) (keyUUID [sealKeyUUIDSize]byte, nonce, ct []byte, ok bool) {
	if len(blob) < SealOverhead || string(blob[:len(sealMagic)]) != sealMagic {
		return keyUUID, nil, nil, false
	}
	copy(keyUUID[:], blob[len(sealMagic):len(sealMagic)+sealKeyUUIDSize])
	nonce = blob[len(sealMagic)+sealKeyUUIDSize : SealOverhead-sealTagSize]
	ct = blob[SealOverhead-sealTagSize:]
	return keyUUID, nonce, ct, true
}

// openPreview opens an NCGOPV1 blob: the header's key UUID is resolved
// through resolve in the caller's ctx (owner session / sharee wrap /
// master-mode anonymous — the same identity paths a content read honors),
// then the ciphertext authenticates against the cacheKey-bound AD. Every
// failure after the magic check — truncation, resolve miss, GCM open —
// wraps encrypt.ErrIntegrity or passes the resolve error through, so callers
// can map it like the equivalent content-read failure.
func openPreview(ctx context.Context, blob []byte, cacheKey string, resolve func(ctx context.Context, keyUUID [sealKeyUUIDSize]byte) ([]byte, error)) (plain []byte, err error) {
	keyUUID, nonce, ct, ok := sealKeyUUID(blob)
	if !ok {
		return nil, fmt.Errorf("preview: sealed blob truncated or missing magic: %w", encrypt.ErrIntegrity)
	}
	fk, err := resolve(ctx, keyUUID)
	if err != nil {
		return nil, err
	}
	aead, err := sealAEAD(sealKey(fk, keyUUID))
	if err != nil {
		return nil, err
	}
	plain, err = aead.Open(nil, nonce, ct, sealAD(keyUUID, cacheKey))
	if err != nil {
		return nil, fmt.Errorf("preview: sealed blob authentication failed: %w", errors.Join(err, encrypt.ErrIntegrity))
	}
	return plain, nil
}

// sealKey derives NK_pv = HKDF-SHA256(FK, salt="", "NCGOPV1" || keyUUID) —
// the same HKDF idiom the encrypt package uses (hkdfSHA256Salt with a nil
// salt), kept local so the preview package never reaches into encrypt
// internals.
func sealKey(fk []byte, keyUUID [sealKeyUUIDSize]byte) []byte {
	info := make([]byte, 0, len(sealMagic)+sealKeyUUIDSize)
	info = append(info, sealMagic...)
	info = append(info, keyUUID[:]...)
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, fk, nil, info)
	if _, err := io.ReadFull(r, out); err != nil {
		// hkdf.Reader errors only when the underlying hash fails; sha256 never does.
		panic(fmt.Sprintf("preview: hkdf: %v", err))
	}
	return out
}

func sealAD(keyUUID [sealKeyUUIDSize]byte, cacheKey string) []byte {
	ad := make([]byte, 0, len(sealMagic)+sealKeyUUIDSize+len(cacheKey))
	ad = append(ad, sealMagic...)
	ad = append(ad, keyUUID[:]...)
	return append(ad, cacheKey...)
}

func sealAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("preview: seal cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("preview: seal gcm: %w", err)
	}
	return aead, nil
}

// sealedRSC serves decrypted blob bytes as an io.ReadSeekCloser.
type sealedRSC struct{ *bytes.Reader }

func (sealedRSC) Close() error { return nil }
