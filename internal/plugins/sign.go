package plugins

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

var (
	ErrSignatureInvalid   = errors.New("plugins: invalid signature")
	ErrSignatureUntrusted = errors.New("plugins: signature key not trusted")
	ErrUnsigned           = errors.New("plugins: archive is unsigned")
)

// Signature is the decoded signature.sig member.
type Signature struct {
	KeyID     string `json:"keyid"`
	Signature string `json:"signature"` // base64 ed25519 signature
}

// KeyIDFromPublic derives the key id shown in signatures: the first 16 hex
// chars of the public key's SHA-256.
func KeyIDFromPublic(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// digestEntry is one member hash in the canonical signed payload.
type digestEntry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// signingPayload builds the canonical byte string that is signed: a JSON
// array of {name, sha256} for every member, sorted by name.
func signingPayload(members map[string][]byte) []byte {
	entries := make([]digestEntry, 0, len(members))
	for name, data := range members {
		sum := sha256.Sum256(data)
		entries = append(entries, digestEntry{Name: name, SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	payload, err := json.Marshal(entries)
	if err != nil { // unreachable: only strings
		panic(err)
	}
	return payload
}

// SignMembers produces a signature.sig member over the given archive members
// (which must include plugin.toml and the module).
func SignMembers(members map[string][]byte, priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: bad private key", ErrSignatureInvalid)
	}
	payload := signingPayload(members)
	sig := ed25519.Sign(priv, payload)
	out, err := json.Marshal(Signature{
		KeyID:     KeyIDFromPublic(priv.Public().(ed25519.PublicKey)), //nolint:errcheck // ed25519 public key type is guaranteed
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignatureInvalid, err)
	}
	return out, nil
}

// VerifyMembers checks raw (a signature.sig member) against the archive
// members and the trusted public keys.
func VerifyMembers(raw []byte, members map[string][]byte, trusted []ed25519.PublicKey) (keyID string, err error) {
	var s Signature
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%w: %w", ErrSignatureInvalid, err)
	}
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: bad signature encoding", ErrSignatureInvalid)
	}
	payload := signingPayload(members)
	for _, pub := range trusted {
		if KeyIDFromPublic(pub) != s.KeyID {
			continue
		}
		if ed25519.Verify(pub, payload, sig) {
			return s.KeyID, nil
		}
		return "", fmt.Errorf("%w: verification failed", ErrSignatureInvalid)
	}
	return "", fmt.Errorf("%w: %s", ErrSignatureUntrusted, s.KeyID)
}

// GenerateKey returns a new ed25519 key pair.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
