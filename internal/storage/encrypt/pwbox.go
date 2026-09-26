package encrypt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// Password-wrapped user keys (ADR-0100 §3, built in ADR-0101): the enrolled
// user's UK is an X25519 keypair. The private key is sealed under a
// password-derived KEK (pw_sealed_uk), file keys are wrapped as ephemeral-ECDH
// boxes (scheme = 1) so the server can wrap FOR an enrolled user with only
// their public key, and a live session carries the private key master-sealed
// (sessions.sealed_uk). Symmetric scheme = 0 wraps (NCGOFK1) are untouched.
const (
	pwSealAD     = "NCGOPW1" // pw_sealed_uk: "NCGOPW1" || be64(user_id)
	boxWrapAD    = "NCGOBX1" // box: "NCGOBX1" || keyUUID(16) || be64(user_id)
	sessionKeyAD = "NCGOSK1" // session copy: "NCGOSK1" || session id bytes

	pwSaltSize    = 16
	x25519KeySize = 32

	// pwSealedUKSize is nonce(12) || AES-256-GCM(privkey(32)) = 60 bytes.
	pwSealedUKSize = nonceSize + x25519KeySize + tagSize
	// boxWrapSize is ephPub(32) || nonce(12) || AES-256-GCM(FK(32)) = 92.
	boxWrapSize = x25519KeySize + nonceSize + fileKeySize + tagSize
)

// KeyDerivationParams is the argon2id parameter set a pw_sealed_uk was
// sealed with, stored PHC-style in user_key_pw.pw_kdf so later KDF-param
// changes never strand rows. The field types mirror auth.Argon2idParams.
type KeyDerivationParams struct {
	MemoryKB    uint32
	Iterations  uint32
	Parallelism uint8
}

// marshalKDF renders params as the pinned pw_kdf TEXT: "m=<KB>,t=<it>,p=<par>".
func (p KeyDerivationParams) marshalKDF() string {
	return "m=" + strconv.FormatUint(uint64(p.MemoryKB), 10) +
		",t=" + strconv.FormatUint(uint64(p.Iterations), 10) +
		",p=" + strconv.FormatUint(uint64(p.Parallelism), 10)
}

// parseKDF parses the pinned "m=...,t=...,p=..." form exactly; unknown or
// malformed strings are an error (a row we cannot interpret must fail
// loudly, never silently derive with guessed parameters).
func parseKDF(s string) (KeyDerivationParams, error) {
	var p KeyDerivationParams
	m, rest, ok := strings.Cut(s, ",")
	if !ok {
		return p, fmt.Errorf("encrypt: pw_kdf %q malformed", s)
	}
	t, par, ok := strings.Cut(rest, ",")
	if !ok {
		return p, fmt.Errorf("encrypt: pw_kdf %q malformed", s)
	}
	if err := kdfField(m, "m", &p.MemoryKB); err != nil {
		return p, err
	}
	if err := kdfField(t, "t", &p.Iterations); err != nil {
		return p, err
	}
	var par64 uint64
	if err := kdfField64(par, "p", &par64); err != nil {
		return p, err
	}
	if par64 == 0 || par64 > 255 {
		return p, fmt.Errorf("encrypt: pw_kdf %q: parallelism out of range", s)
	}
	p.Parallelism = uint8(par64)
	if p.MemoryKB == 0 || p.Iterations == 0 {
		return p, fmt.Errorf("encrypt: pw_kdf %q: memory and iterations must be positive", s)
	}
	return p, nil
}

func kdfField(field, name string, dst *uint32) error {
	var v uint64
	if err := kdfField64(field, name, &v); err != nil {
		return err
	}
	*dst = uint32(v)
	return nil
}

func kdfField64(field, name string, dst *uint64) error {
	key, val, ok := strings.Cut(field, "=")
	if !ok || key != name || val == "" {
		return fmt.Errorf("encrypt: pw_kdf field %q malformed", field)
	}
	n, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return fmt.Errorf("encrypt: pw_kdf field %q: %w", field, err)
	}
	*dst = n
	return nil
}

// passwordKEK derives the 32-byte password KEK: argon2id(password, pw_salt,
// stored params).
func passwordKEK(password string, salt []byte, p KeyDerivationParams) []byte {
	return argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKB, p.Parallelism, 32)
}

// pwAD is the associated data binding a pw_sealed_uk to its owner:
// "NCGOPW1" || bigendian-uint64(user_id).
func pwAD(userID int64) []byte {
	ad := make([]byte, 0, len(pwSealAD)+8)
	ad = append(ad, pwSealAD...)
	//nolint:gosec // G115: user IDs are positive serial keys
	return binary.BigEndian.AppendUint64(ad, uint64(userID))
}

// sealPrivForPassword seals priv under the password KEK with a fresh 16-byte
// salt and the given KDF params, returning the 60-byte pw_sealed_uk blob and
// the salt (both persisted on the user_key_pw row).
func sealPrivForPassword(priv []byte, password string, userID int64, p KeyDerivationParams) (sealed, salt []byte, err error) {
	if len(priv) != x25519KeySize {
		return nil, nil, fmt.Errorf("encrypt: private key must be %d bytes, got %d", x25519KeySize, len(priv))
	}
	salt = make([]byte, pwSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("encrypt: generate pw salt: %w", err)
	}
	sealed, err = wrapSeal(passwordKEK(password, salt, p), priv, pwAD(userID))
	if err != nil {
		return nil, nil, err
	}
	return sealed, salt, nil
}

// openPrivWithPassword reverses sealPrivForPassword.
func openPrivWithPassword(sealed []byte, password string, salt []byte, userID int64, p KeyDerivationParams) ([]byte, error) {
	return wrapOpen(passwordKEK(password, salt, p), sealed, pwAD(userID))
}

// newKeypair mints an enrolled user's X25519 keypair: 32 random private
// bytes and pub = X25519(priv, basepoint).
func newKeypair() (priv, pub []byte, err error) {
	priv = make([]byte, x25519KeySize)
	if _, err := rand.Read(priv); err != nil {
		return nil, nil, fmt.Errorf("encrypt: generate keypair: %w", err)
	}
	pub, err = curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt: derive public key: %w", err)
	}
	return priv, pub, nil
}

// boxAD is the HKDF info and AEAD associated data binding a box wrap to its
// file key UUID and recipient: "NCGOBX1" || keyUUID(16) || be64(user_id).
func boxAD(keyUUID [keyUUIDSize]byte, userID int64) []byte {
	ad := make([]byte, 0, len(boxWrapAD)+keyUUIDSize+8)
	ad = append(ad, boxWrapAD...)
	ad = append(ad, keyUUID[:]...)
	//nolint:gosec // G115: user IDs are positive serial keys
	return binary.BigEndian.AppendUint64(ad, uint64(userID))
}

// boxWrap seals fk for the recipient's X25519 public key (scheme = 1, 92
// bytes): an ephemeral keypair ECDH-shares with recipientPub, the shared
// secret is HKDF-SHA256-stretched (salt=nil, info = boxAD) into the wrap
// key, and the blob is ephPub(32) || nonce(12) || AES-256-GCM(wrapKey, fk,
// boxAD). Ephemeral ECDH needs no sender authentication — wraps are created
// exclusively server-side.
func boxWrap(recipientPub, fk []byte, keyUUID [keyUUIDSize]byte, userID int64) ([]byte, error) {
	if len(recipientPub) != x25519KeySize {
		return nil, fmt.Errorf("encrypt: recipient public key must be %d bytes, got %d", x25519KeySize, len(recipientPub))
	}
	if len(fk) != fileKeySize {
		return nil, fmt.Errorf("encrypt: file key must be %d bytes, got %d", fileKeySize, len(fk))
	}
	ephPriv, ephPub, err := newKeypair()
	if err != nil {
		return nil, err
	}
	shared, err := curve25519.X25519(ephPriv, recipientPub)
	if err != nil {
		return nil, fmt.Errorf("encrypt: box ECDH: %w", err)
	}
	ad := boxAD(keyUUID, userID)
	aead, err := newAEAD(hkdfSHA256(shared, ad))
	if err != nil {
		return nil, err
	}
	n := make([]byte, nonceSize)
	if _, err := rand.Read(n); err != nil {
		return nil, fmt.Errorf("encrypt: generate box nonce: %w", err)
	}
	blob := make([]byte, 0, boxWrapSize)
	blob = append(blob, ephPub...)
	blob = append(blob, n...)
	return aead.Seal(blob, n, fk, ad), nil
}

// boxOpen reverses boxWrap with the recipient's private key: the ephemeral
// public key comes out of the blob, the shared secret is recomputed, and
// authentication failures mean a tampered box or the wrong recipient key.
func boxOpen(recipientPriv, blob []byte, keyUUID [keyUUIDSize]byte, userID int64) ([]byte, error) {
	if len(recipientPriv) != x25519KeySize {
		return nil, fmt.Errorf("encrypt: recipient private key must be %d bytes, got %d", x25519KeySize, len(recipientPriv))
	}
	if len(blob) != boxWrapSize {
		return nil, fmt.Errorf("encrypt: box wrap truncated (%d bytes, want %d)", len(blob), boxWrapSize)
	}
	shared, err := curve25519.X25519(recipientPriv, blob[:x25519KeySize])
	if err != nil {
		return nil, fmt.Errorf("encrypt: box ECDH: %w", err)
	}
	ad := boxAD(keyUUID, userID)
	aead, err := newAEAD(hkdfSHA256(shared, ad))
	if err != nil {
		return nil, err
	}
	fk, err := aead.Open(nil, blob[x25519KeySize:x25519KeySize+nonceSize], blob[x25519KeySize+nonceSize:], ad)
	if err != nil {
		return nil, fmt.Errorf("encrypt: box wrap authentication failed: %w", err)
	}
	return fk, nil
}

// hkdfSHA256 is HKDF-SHA256(secret, salt=nil, info) → 32 bytes.
func hkdfSHA256(secret, info []byte) []byte {
	return hkdfSHA256Salt(secret, nil, info)
}

// hkdfSHA256Salt is HKDF-SHA256(secret, salt, info) → 32 bytes.
func hkdfSHA256Salt(secret, salt, info []byte) []byte {
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, secret, salt, info)
	if _, err := io.ReadFull(r, out); err != nil {
		// hkdf.Reader errors only when the underlying hash fails; sha256 never does.
		panic(fmt.Sprintf("encrypt: hkdf: %v", err))
	}
	return out
}

// sealSessionCopy seals an unlocked private key for storage on a session
// row: keyID(1) || nonce(12) || AES-256-GCM(ring[keyID], priv, "NCGOSK1" ||
// session-id bytes). Sealing under the current ring position and recording
// its ID byte keeps the copy openable after a master-key rotation while the
// ring stays append-only (ADR-0101 refinement of ADR-0100 §3, which
// specified no key ID byte). SQLResolver.SealSessionKey exposes it.
func (r *SQLResolver) sealSessionCopy(priv []byte, sessionID string) ([]byte, error) {
	if len(priv) != x25519KeySize {
		return nil, fmt.Errorf("encrypt: private key must be %d bytes, got %d", x25519KeySize, len(priv))
	}
	keyID := len(r.keys) - 1
	sealed, err := wrapSeal(r.keys[keyID], priv, sessionAD(sessionID))
	if err != nil {
		return nil, err
	}
	blob := make([]byte, 0, 1+len(sealed))
	//nolint:gosec // G115: the constructor caps the ring at MaxKeys, so the key ID always fits a byte
	blob = append(blob, byte(keyID))
	return append(blob, sealed...), nil
}

// openSessionCopy reverses sealSessionCopy: the recorded key-ID byte picks
// the ring key (bounds-checked), and an authentication failure means a
// corrupt copy or a mismatched keyring — ErrIntegrity, never a silent
// degrade. SQLResolver.UnsealSessionKey exposes it.
func (r *SQLResolver) openSessionCopy(sessionID string, blob []byte) ([]byte, error) {
	if len(blob) < 1+nonceSize+tagSize {
		return nil, fmt.Errorf("encrypt: session key copy truncated (%d bytes): %w", len(blob), ErrIntegrity)
	}
	keyID := int(blob[0])
	if keyID >= len(r.keys) {
		return nil, fmt.Errorf("encrypt: session key copy sealed under key id %d, keyring holds %d keys: %w",
			keyID, len(r.keys), ErrIntegrity)
	}
	priv, err := wrapOpen(r.keys[keyID], blob[1:], sessionAD(sessionID))
	if err != nil {
		return nil, fmt.Errorf("encrypt: session key copy authentication failed: %w", errors.Join(err, ErrIntegrity))
	}
	return priv, nil
}

// sessionAD is the associated data binding a session key copy to its
// session: "NCGOSK1" || session-id bytes.
func sessionAD(sessionID string) []byte {
	ad := make([]byte, 0, len(sessionKeyAD)+len(sessionID))
	ad = append(ad, sessionKeyAD...)
	return append(ad, sessionID...)
}
