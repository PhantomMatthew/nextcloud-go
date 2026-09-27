package encrypt

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// NCGOFN1 name tokens (ADR-0104 §2): parent-keyed deterministic filename
// encryption. A directory's 32-byte directory key (DK — an FK whose files
// row is a directory) derives a name key NK for its children; each child's
// name seals under NK with a SIV-style synthetic nonce, so the same name
// under the same parent always yields the same token. Determinism is what
// keeps the whole path-string SQL surface working: request-time resolution
// re-encrypts segment by segment into exact lookups, and UNIQUE(user_id,
// path) keeps enforcing collisions. The associated data binds the token to
// the parent key UUID, so a token replayed under a different folder fails
// authentication.
//
//	NK    = HKDF-SHA256(ikm = DK, salt = "", info = "NCGONK1" || parentKeyUUID)
//	nonce = HMAC-SHA256(NK, "NCGOFN1" || 0x00 || nameBytes)[0:12]
//	blob  = nonce || AES-256-GCM(NK, nonce, nameBytes, "NCGOFN1" || parentKeyUUID)
//	token = base64.RawURLEncoding(blob)                                    // 12+N+16 B raw
const (
	// NameSchemeNCGOFN1 is the files.name_scheme/users.name_scheme value
	// marking NCGOFN1-tokenized names (0 = plaintext).
	NameSchemeNCGOFN1 = 1

	nameKeySize = 32 // NK; DKs share the FK size
	nameKeyInfo = "NCGONK1"
	nameTokenAD = "NCGOFN1"
)

// DeriveNameKey derives the 32-byte name key for the children of the
// directory whose key UUID is parentKeyUUID:
// HKDF-SHA256(ikm = dk, salt = "", info = "NCGONK1" || parentKeyUUID).
// dk must be the parent's 32-byte directory key.
func DeriveNameKey(dk []byte, parentKeyUUID [16]byte) ([]byte, error) {
	if len(dk) != nameKeySize {
		return nil, fmt.Errorf("encrypt: name key: directory key must be %d bytes, got %d", nameKeySize, len(dk))
	}
	info := make([]byte, 0, len(nameKeyInfo)+keyUUIDSize)
	info = append(info, nameKeyInfo...)
	info = append(info, parentKeyUUID[:]...)
	return hkdfSHA256(dk, info), nil
}

// EncryptName maps a plaintext child name to its deterministic NCGOFN1 token
// under the parent directory's name key: the synthetic nonce
// HMAC-SHA256(nk, "NCGOFN1" || 0x00 || name)[0:12] prefixes
// AES-256-GCM(nk, nonce, name, ad = "NCGOFN1" || parentKeyUUID) and the blob
// is base64url-encoded without padding. Names are byte-oriented — no Unicode
// normalization, no case folding. The empty name is rejected: the root is
// never encrypted.
//
//nolint:revive // ADR-0104 pins the exported name; the package stutter is accepted
func EncryptName(nk []byte, parentKeyUUID [16]byte, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("encrypt: name token: empty name (the root is never encrypted)")
	}
	if len(nk) != nameKeySize {
		return "", fmt.Errorf("encrypt: name token: name key must be %d bytes, got %d", nameKeySize, len(nk))
	}
	mac := hmac.New(sha256.New, nk)
	_, _ = mac.Write([]byte(nameTokenAD)) // hash.Hash.Write never errors
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(name))
	nonce := mac.Sum(nil)[:nonceSize]
	aead, err := newAEAD(nk)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(name), nameAD(parentKeyUUID))), nil
}

// DecryptName reverses EncryptName. Every failure — bad base64, a truncated
// blob, a GCM open miss (wrong name key, wrong parent key UUID, tampered
// token) — wraps ErrIntegrity: a token that does not authenticate is
// corruption, never a plaintext fallback.
func DecryptName(nk []byte, parentKeyUUID [16]byte, token string) (string, error) {
	blob, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("encrypt: name token decode: %w", errors.Join(err, ErrIntegrity))
	}
	if len(blob) <= nonceSize+tagSize {
		return "", fmt.Errorf("encrypt: name token truncated (%d bytes): %w", len(blob), ErrIntegrity)
	}
	aead, err := newAEAD(nk)
	if err != nil {
		return "", fmt.Errorf("encrypt: name token: %w", errors.Join(err, ErrIntegrity))
	}
	plain, err := aead.Open(nil, blob[:nonceSize], blob[nonceSize:], nameAD(parentKeyUUID))
	if err != nil {
		return "", fmt.Errorf("encrypt: name token authentication failed: %w", errors.Join(err, ErrIntegrity))
	}
	return string(plain), nil
}

// nameAD is the AEAD associated data binding a name token to its parent key
// UUID: "NCGOFN1" || parentKeyUUID(16 raw bytes).
func nameAD(parentKeyUUID [keyUUIDSize]byte) []byte {
	ad := make([]byte, 0, len(nameTokenAD)+keyUUIDSize)
	ad = append(ad, nameTokenAD...)
	return append(ad, parentKeyUUID[:]...)
}

// NCGOSP1 sealed paths (ADR-0104 §7, phase 3a): a share row carries a
// share-scoped copy of the plaintext absolute owner path, sealed under the
// share target's own key (the directory key of a folder target, the file key
// of a file target) with a RANDOM nonce — unlike NCGOFN1 name tokens the
// value is never addressed by SQL, so determinism buys nothing and the
// random nonce keeps identical paths from correlating across shares.
//
//	blob = nonce(12) || AES-256-GCM(key, nonce, plainPath, ad = "NCGOSP1" || keyUUID)
//	seal = base64.RawURLEncoding(blob)
const namePathAD = "NCGOSP1"

// SealPath seals a plaintext absolute path under key (the share target's
// 32-byte directory or file key), bound to keyUUID as associated data.
func SealPath(key []byte, keyUUID [16]byte, plain string) (string, error) {
	if len(key) != fileKeySize {
		return "", fmt.Errorf("encrypt: seal path: key must be %d bytes, got %d", fileKeySize, len(key))
	}
	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("encrypt: seal path: nonce: %w", err)
	}
	ad := make([]byte, 0, len(namePathAD)+keyUUIDSize)
	ad = append(ad, namePathAD...)
	ad = append(ad, keyUUID[:]...)
	blob := aead.Seal(nonce, nonce, []byte(plain), ad)
	return base64.RawURLEncoding.EncodeToString(blob), nil
}

// OpenPath reverses SealPath. Every failure — bad base64, a truncated blob,
// a GCM open miss (wrong key, wrong key UUID, tampered seal) — wraps
// ErrIntegrity: a seal that does not authenticate is corruption, never a
// plaintext fallback.
func OpenPath(key []byte, keyUUID [16]byte, blob string) (string, error) {
	if len(key) != fileKeySize {
		return "", fmt.Errorf("encrypt: open path: key must be %d bytes, got %d: %w", fileKeySize, len(key), ErrIntegrity)
	}
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		return "", fmt.Errorf("encrypt: open path decode: %w", errors.Join(err, ErrIntegrity))
	}
	if len(raw) <= nonceSize+tagSize {
		return "", fmt.Errorf("encrypt: open path truncated (%d bytes): %w", len(raw), ErrIntegrity)
	}
	aead, err := newAEAD(key)
	if err != nil {
		return "", fmt.Errorf("encrypt: open path: %w", errors.Join(err, ErrIntegrity))
	}
	ad := make([]byte, 0, len(namePathAD)+keyUUIDSize)
	ad = append(ad, namePathAD...)
	ad = append(ad, keyUUID[:]...)
	plain, err := aead.Open(nil, raw[:nonceSize], raw[nonceSize:], ad)
	if err != nil {
		return "", fmt.Errorf("encrypt: open path authentication failed: %w", errors.Join(err, ErrIntegrity))
	}
	return string(plain), nil
}
