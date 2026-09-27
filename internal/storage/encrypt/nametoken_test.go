package encrypt

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Fixed inputs for the frozen NCGOFN1 vectors: DK = bytes 0x00..0x1f,
// parent key UUID = bytes 0xa0..0xaf.
func nameTestVectors(t *testing.T) (dk []byte, parent [16]byte, nk []byte) {
	t.Helper()
	dk = make([]byte, 32)
	for i := range dk {
		dk[i] = byte(i)
	}
	for i := range parent {
		parent[i] = byte(0xa0 + i)
	}
	nk, err := DeriveNameKey(dk, parent)
	if err != nil {
		t.Fatal(err)
	}
	return dk, parent, nk
}

// TestNCGOFN1FrozenVectors pins the ADR-0104 §2 construction byte-exact:
// fixed (DK, parent key UUID, name) → fixed token. Changing an expected
// value means changing the on-disk format — do so only with a migration.
func TestNCGOFN1FrozenVectors(t *testing.T) {
	_, parent, nk := nameTestVectors(t)
	if got, want := hex.EncodeToString(nk), "1404e742dcc9a5f2675db9bcb3487904eb5969ea7c7f2476e3aac9379dda7c66"; got != want {
		t.Fatalf("NK = %s, want %s", got, want)
	}
	vectors := []struct {
		name  string
		token string
	}{
		{"a", "rqZrAhgnaEN35P61yNTBQ3UuflDzpi5AUWAqpjg"},
		{"Photos", "qZWZpJKpWz_8nGJnVovb1X67-6Fitow0FSkVwM83aEwduQ"},
		{"2026-裁员名单.xlsx", "roymrFSmPkViy4_10QDSBJwwPvON_7tS94zFQ8zsseHAQOb7KkjfSdPugIrleR_kObc"},
	}
	for _, v := range vectors {
		tok, err := EncryptName(nk, parent, v.name)
		if err != nil {
			t.Fatalf("%q: %v", v.name, err)
		}
		if tok != v.token {
			t.Errorf("%q: token = %q, want %q", v.name, tok, v.token)
		}
		back, err := DecryptName(nk, parent, tok)
		if err != nil {
			t.Fatalf("%q: decrypt: %v", v.name, err)
		}
		if back != v.name {
			t.Errorf("%q: round trip = %q", v.name, back)
		}
	}
}

func TestNCGOFN1Deterministic(t *testing.T) {
	_, parent, nk := nameTestVectors(t)
	a, err := EncryptName(nk, parent, "budget.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncryptName(nk, parent, "budget.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("same input tokenized twice: %q != %q", a, b)
	}

	// The same name under a different parent key UUID is a different token —
	// cross-folder name equality stays hidden.
	other := parent
	other[0] ^= 0xff
	otherNK, err := DeriveNameKey(mustDK(t), other)
	if err != nil {
		t.Fatal(err)
	}
	c, err := EncryptName(otherNK, other, "budget.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Error("same name under different parents produced the same token")
	}
}

func mustDK(t *testing.T) []byte {
	t.Helper()
	dk, _, _ := nameTestVectors(t)
	return dk
}

func TestNCGOFN1FailuresAreIntegrity(t *testing.T) {
	dk, parent, nk := nameTestVectors(t)
	tok, err := EncryptName(nk, parent, "Photos")
	if err != nil {
		t.Fatal(err)
	}

	// Wrong NK (derived from a different DK).
	otherDK := make([]byte, 32)
	for i := range otherDK {
		otherDK[i] = byte(i + 1)
	}
	wrongNK, err := DeriveNameKey(otherDK, parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptName(wrongNK, parent, tok); !errors.Is(err, ErrIntegrity) {
		t.Errorf("wrong NK err = %v, want ErrIntegrity", err)
	}

	// Wrong parent key UUID (AD mismatch).
	otherParent := parent
	otherParent[15] ^= 0x01
	if _, err := DecryptName(nk, otherParent, tok); !errors.Is(err, ErrIntegrity) {
		t.Errorf("wrong parent UUID err = %v, want ErrIntegrity", err)
	}

	// Tampered token (flip a ciphertext byte: same length, valid base64).
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatal(err)
	}
	raw[nonceSize] ^= 0x01
	if _, err := DecryptName(nk, parent, base64.RawURLEncoding.EncodeToString(raw)); !errors.Is(err, ErrIntegrity) {
		t.Errorf("tampered token err = %v, want ErrIntegrity", err)
	}

	// Bad base64 and truncated blobs.
	if _, err := DecryptName(nk, parent, "not!base64!"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("bad base64 err = %v, want ErrIntegrity", err)
	}
	if _, err := DecryptName(nk, parent, base64.RawURLEncoding.EncodeToString(raw[:nonceSize+tagSize])); !errors.Is(err, ErrIntegrity) {
		t.Errorf("truncated blob err = %v, want ErrIntegrity", err)
	}

	// Input validation: the DK/NK lengths and the empty name.
	if _, err := DeriveNameKey(dk[:31], parent); err == nil {
		t.Error("31-byte DK accepted")
	}
	if _, err := EncryptName(nk[:31], parent, "x"); err == nil {
		t.Error("31-byte NK accepted")
	}
	if _, err := EncryptName(nk, parent, ""); err == nil {
		t.Error("empty name accepted (the root is never encrypted)")
	}
}

func TestNCGOFN1RoundTripShapes(t *testing.T) {
	_, parent, nk := nameTestVectors(t)

	// Non-UTF8 name bytes pass through exactly (names are byte-oriented).
	binaryName := string([]byte{0xff, 0xfe, 'x', 0x80})
	tok, err := EncryptName(nk, parent, binaryName)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecryptName(nk, parent, tok)
	if err != nil {
		t.Fatal(err)
	}
	if back != binaryName {
		t.Errorf("non-UTF8 round trip = %q, want %q", back, binaryName)
	}

	// A 255-rune name (the plaintext name budget, ADR-0104 §3) round-trips.
	longName := strings.Repeat("界", 255)
	tok, err = EncryptName(nk, parent, longName)
	if err != nil {
		t.Fatal(err)
	}
	back, err = DecryptName(nk, parent, tok)
	if err != nil {
		t.Fatal(err)
	}
	if back != longName {
		t.Errorf("255-rune round trip: %d runes back", len([]rune(back)))
	}

	// Token length is pinned: base64url of nonce(12) || N || tag(16).
	for _, name := range []string{"a", "Photos", "2026-裁员名单.xlsx", longName} {
		tok, err := EncryptName(nk, parent, name)
		if err != nil {
			t.Fatal(err)
		}
		want := base64.RawURLEncoding.EncodedLen(nonceSize + len(name) + tagSize)
		if len(tok) != want {
			t.Errorf("%d-byte name: token = %d chars, want %d", len(name), len(tok), want)
		}
	}
}

// TestNCGOSP1SealPathRoundTrip pins the SealPath/OpenPath contract (ADR-0104
// §7): random nonces make frozen vectors impossible, so the pin is the
// round-trip plus non-determinism of repeat seals.
func TestNCGOSP1SealPathRoundTrip(t *testing.T) {
	dk, parent, _ := nameTestVectors(t)
	plain := "/Photos/2026/裁员名单.xlsx"
	seal, err := SealPath(dk, parent, plain)
	if err != nil {
		t.Fatal(err)
	}
	back, err := OpenPath(dk, parent, seal)
	if err != nil {
		t.Fatal(err)
	}
	if back != plain {
		t.Errorf("round trip = %q, want %q", back, plain)
	}
	// Random nonce: sealing the same path twice yields different blobs.
	seal2, err := SealPath(dk, parent, plain)
	if err != nil {
		t.Fatal(err)
	}
	if seal == seal2 {
		t.Error("repeat seals identical, want random nonces")
	}
	// Seal length is pinned: base64url of nonce(12) || N || tag(16).
	if want := base64.RawURLEncoding.EncodedLen(nonceSize + len(plain) + tagSize); len(seal) != want {
		t.Errorf("seal = %d chars, want %d", len(seal), want)
	}
}

// TestNCGOSP1FailuresAreIntegrity pins that every open failure — wrong key,
// wrong key UUID (AD binding), tamper, bad encoding — wraps ErrIntegrity.
func TestNCGOSP1FailuresAreIntegrity(t *testing.T) {
	dk, parent, _ := nameTestVectors(t)
	seal, err := SealPath(dk, parent, "/a/b.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Wrong key.
	otherKey := make([]byte, 32)
	for i := range otherKey {
		otherKey[i] = byte(i + 1)
	}
	if _, err := OpenPath(otherKey, parent, seal); !errors.Is(err, ErrIntegrity) {
		t.Errorf("wrong key err = %v, want ErrIntegrity", err)
	}

	// Wrong key UUID (AD binding: a seal opened under another uuid fails).
	otherUUID := parent
	otherUUID[15] ^= 0x01
	if _, err := OpenPath(dk, otherUUID, seal); !errors.Is(err, ErrIntegrity) {
		t.Errorf("wrong uuid err = %v, want ErrIntegrity", err)
	}

	// Tampered seal (flip a ciphertext byte: same length, valid base64).
	raw, err := base64.RawURLEncoding.DecodeString(seal)
	if err != nil {
		t.Fatal(err)
	}
	raw[nonceSize] ^= 0x01
	if _, err := OpenPath(dk, parent, base64.RawURLEncoding.EncodeToString(raw)); !errors.Is(err, ErrIntegrity) {
		t.Errorf("tampered seal err = %v, want ErrIntegrity", err)
	}

	// Bad base64, truncated blob, wrong key length.
	if _, err := OpenPath(dk, parent, "not!base64!"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("bad base64 err = %v, want ErrIntegrity", err)
	}
	if _, err := OpenPath(dk, parent, base64.RawURLEncoding.EncodeToString(raw[:nonceSize+tagSize])); !errors.Is(err, ErrIntegrity) {
		t.Errorf("truncated blob err = %v, want ErrIntegrity", err)
	}
	if _, err := OpenPath(dk[:31], parent, seal); !errors.Is(err, ErrIntegrity) {
		t.Errorf("31-byte key err = %v, want ErrIntegrity", err)
	}
	if _, err := SealPath(dk[:31], parent, "/a"); err == nil {
		t.Error("seal with 31-byte key accepted")
	}
}
