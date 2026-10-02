// Package mail implements the v2 Mail epic (ADR-0108): per-user IMAP/SMTP
// accounts with sealed-at-rest credentials, a JSON REST API mirroring the
// official Nextcloud Mail app subset, and (later increments) mailbox sync
// and send. M1 landed accounts; M2 adds the internal/mail/imap client and
// verifies the IMAP LOGIN before an account is created or its connection
// fields change.
package mail

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Credential sealing (ADR-0108 §3) is deliberately standalone: it derives
// from the instance secret only, so it works whether or not the
// per-user-keys encryption module (internal/storage/encrypt) is enabled —
// background sync must open credentials without any user's unlocked key.
//
// Blob format: salt(16) || nonce(12) || AES-256-GCM ciphertext.
// Key = HKDF-SHA256(instanceSecret, salt, "NCGO mail credential v1").
// AdditionalData binds the blob to its owning identity triple:
// "NCGOMK1" || 0x00 || userID || 0x00 || imapHost || 0x00 || imapUser —
// a blob copied to another user or host does not open.
const (
	credentialSaltSize  = 16
	credentialNonceSize = 12
	credentialKeySize   = 32
	//nolint:gosec // G101: HKDF info label, not a credential
	credentialHKDFInfo = "NCGO mail credential v1"
	credentialADPrefix = "NCGOMK1"
)

// ErrCredential reports a sealed credential that is truncated, sealed under
// a different secret, or bound to different associated data — one
// undifferentiated integrity failure, never a partial decrypt.
var ErrCredential = errors.New("mail: sealed credential invalid")

func credentialAD(userID, imapHost, imapUser string) []byte {
	ad := make([]byte, 0, len(credentialADPrefix)+3+len(userID)+len(imapHost)+len(imapUser))
	ad = append(ad, credentialADPrefix...)
	ad = append(ad, 0x00)
	ad = append(ad, userID...)
	ad = append(ad, 0x00)
	ad = append(ad, imapHost...)
	ad = append(ad, 0x00)
	ad = append(ad, imapUser...)
	return ad
}

func credentialKey(secret string, salt []byte) ([]byte, error) {
	key := make([]byte, credentialKeySize)
	r := hkdf.New(sha256.New, []byte(secret), salt, []byte(credentialHKDFInfo))
	if _, err := io.ReadFull(r, key); err != nil {
		// hkdf.Reader errors only when the underlying hash fails; sha256 never does.
		return nil, fmt.Errorf("mail: hkdf: %w", err)
	}
	return key, nil
}

// SealCredential encrypts plaintext for (userID, imapHost, imapUser) under
// a key derived from the instance secret. The secret must be non-empty —
// the app wiring guarantees it (generated at boot when unconfigured).
func SealCredential(secret, userID, imapHost, imapUser string, plaintext []byte) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("mail: empty instance secret")
	}
	salt := make([]byte, credentialSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("mail: credential salt: %w", err)
	}
	key, err := credentialKey(secret, salt)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, credentialNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("mail: credential nonce: %w", err)
	}
	blob := make([]byte, 0, credentialSaltSize+credentialNonceSize+len(plaintext)+gcm.Overhead())
	blob = append(blob, salt...)
	blob = append(blob, nonce...)
	return gcm.Seal(blob, nonce, plaintext, credentialAD(userID, imapHost, imapUser)), nil
}

// OpenCredential reverses SealCredential; any mismatch — short blob, wrong
// secret, wrong identity triple, corrupted ciphertext — is ErrCredential.
func OpenCredential(secret, userID, imapHost, imapUser string, sealed []byte) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("mail: empty instance secret")
	}
	if len(sealed) < credentialSaltSize+credentialNonceSize {
		return nil, ErrCredential
	}
	salt := sealed[:credentialSaltSize]
	nonce := sealed[credentialSaltSize : credentialSaltSize+credentialNonceSize]
	ct := sealed[credentialSaltSize+credentialNonceSize:]
	key, err := credentialKey(secret, salt)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	if len(ct) < gcm.Overhead() {
		return nil, ErrCredential
	}
	plain, err := gcm.Open(nil, nonce, ct, credentialAD(userID, imapHost, imapUser))
	if err != nil {
		return nil, ErrCredential
	}
	return plain, nil
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("mail: credential cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("mail: credential gcm: %w", err)
	}
	return gcm, nil
}
