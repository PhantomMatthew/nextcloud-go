package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argon2idVersion = argon2.Version
	defaultSaltLen  = 16
	defaultKeyLen   = 32
)

// ErrInvalidHash is returned when a stored password hash is not a valid PHC string.
var ErrInvalidHash = errors.New("auth: invalid password hash")

// PasswordHasher hashes and verifies passwords.
type PasswordHasher interface {
	Hash(pw string) (string, error)
	Verify(hash, pw string) (bool, error)
	NeedsRehash(hash string) bool
}

// Argon2idParams is the Argon2id parameter set.
type Argon2idParams struct {
	MemoryKB    uint32
	Iterations  uint32
	Parallelism uint8
	SaltLen     uint32
	KeyLen      uint32
}

// Argon2id implements PasswordHasher using the PHC string format.
type Argon2id struct {
	params Argon2idParams
}

// NewArgon2id returns a hasher. Zero SaltLen/KeyLen become 16/32.
func NewArgon2id(p Argon2idParams) *Argon2id {
	if p.SaltLen == 0 {
		p.SaltLen = defaultSaltLen
	}
	if p.KeyLen == 0 {
		p.KeyLen = defaultKeyLen
	}
	return &Argon2id{params: p}
}

func (a *Argon2id) Hash(pw string) (string, error) {
	salt := make([]byte, a.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, a.params.Iterations, a.params.MemoryKB, a.params.Parallelism, a.params.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2idVersion, a.params.MemoryKB, a.params.Iterations, a.params.Parallelism,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func (a *Argon2id) Verify(hash, pw string) (bool, error) {
	p, salt, key, err := parsePHC(hash)
	if err != nil {
		return false, err
	}
	keyLen, err := boundedU32(len(key))
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, p.Iterations, p.MemoryKB, p.Parallelism, keyLen)
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, nil
	}
	return true, nil
}

func (a *Argon2id) NeedsRehash(hash string) bool {
	p, _, key, err := parsePHC(hash)
	if err != nil {
		return true
	}
	keyLen, err := boundedU32(len(key))
	if err != nil {
		return true
	}
	return p.MemoryKB != a.params.MemoryKB ||
		p.Iterations != a.params.Iterations ||
		p.Parallelism != a.params.Parallelism ||
		keyLen != a.params.KeyLen
}

func parsePHC(hash string) (Argon2idParams, []byte, []byte, error) {
	// $argon2id$v=19$m=65536,t=3,p=4$salt$hash
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return Argon2idParams{}, nil, nil, ErrInvalidHash
	}
	if !strings.HasPrefix(parts[2], "v=") {
		return Argon2idParams{}, nil, nil, ErrInvalidHash
	}
	ver, err := strconv.Atoi(strings.TrimPrefix(parts[2], "v="))
	if err != nil || ver != argon2idVersion {
		return Argon2idParams{}, nil, nil, ErrInvalidHash
	}
	var p Argon2idParams
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return Argon2idParams{}, nil, nil, ErrInvalidHash
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Argon2idParams{}, nil, nil, ErrInvalidHash
		}
		switch k {
		case "m":
			p.MemoryKB = uint32(n)
		case "t":
			p.Iterations = uint32(n)
		case "p":
			if n > 255 {
				return Argon2idParams{}, nil, nil, ErrInvalidHash
			}
			p.Parallelism = uint8(n)
		default:
			return Argon2idParams{}, nil, nil, ErrInvalidHash
		}
	}
	if p.MemoryKB == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		return Argon2idParams{}, nil, nil, ErrInvalidHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Argon2idParams{}, nil, nil, ErrInvalidHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return Argon2idParams{}, nil, nil, ErrInvalidHash
	}
	saltLen, err := boundedU32(len(salt))
	if err != nil {
		return Argon2idParams{}, nil, nil, err
	}
	keyLen, err := boundedU32(len(key))
	if err != nil {
		return Argon2idParams{}, nil, nil, err
	}
	p.SaltLen = saltLen
	p.KeyLen = keyLen
	return p, salt, key, nil
}

func boundedU32(n int) (uint32, error) {
	if n < 0 || n > 1024*1024 {
		return 0, ErrInvalidHash
	}
	return uint32(n), nil
}
