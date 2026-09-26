package encrypt

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// TestPasswordSealRoundTrip pins the pw_sealed_uk construction (ADR-0100 §3):
// nonce(12) || AES-256-GCM(argon2id KEK, privkey(32), "NCGOPW1" || be64(uid)).
func TestPasswordSealRoundTrip(t *testing.T) {
	params := KeyDerivationParams{MemoryKB: 1024, Iterations: 1, Parallelism: 1}
	priv, _, err := newKeypair()
	if err != nil {
		t.Fatal(err)
	}
	sealed, salt, err := sealPrivForPassword(priv, "correct horse", 7, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != pwSealedUKSize {
		t.Fatalf("pw_sealed_uk = %d bytes, want %d", len(sealed), pwSealedUKSize)
	}
	if len(salt) != pwSaltSize {
		t.Fatalf("pw_salt = %d bytes, want %d", len(salt), pwSaltSize)
	}
	got, err := openPrivWithPassword(sealed, "correct horse", salt, 7, params)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv) {
		t.Fatal("round-trip changed the private key")
	}

	// Wrong password, wrong user (AD), a tampered byte, and wrong stored
	// params all fail authentication — a pw_sealed_uk is bound to exactly
	// its password, owner, and KDF parameters.
	if _, err := openPrivWithPassword(sealed, "wrong", salt, 7, params); err == nil {
		t.Error("wrong password opened the seal")
	}
	if _, err := openPrivWithPassword(sealed, "correct horse", salt, 8, params); err == nil {
		t.Error("wrong user id opened the seal (AD not bound)")
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := openPrivWithPassword(tampered, "correct horse", salt, 7, params); err == nil {
		t.Error("tampered seal opened")
	}
	otherParams := KeyDerivationParams{MemoryKB: 2048, Iterations: 1, Parallelism: 1}
	if _, err := openPrivWithPassword(sealed, "correct horse", salt, 7, otherParams); err == nil {
		t.Error("wrong KDF params opened the seal")
	}
}

// TestKeypair pins the X25519 keypair shape: 32-byte private key, and the
// public key is X25519(priv, basepoint) — two mints never collide.
func TestKeypair(t *testing.T) {
	priv, pub, err := newKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != x25519KeySize || len(pub) != x25519KeySize {
		t.Fatalf("keypair sizes = %d/%d, want 32/32", len(priv), len(pub))
	}
	_, pub2, err := newKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(pub, pub2) {
		t.Fatal("two mints produced the same public key")
	}
}

// TestBoxWrapRoundTrip pins the scheme=1 box construction (ADR-0100 §3):
// ephPub(32) || nonce(12) || AES-256-GCM(HKDF(shared, info), FK,
// "NCGOBX1" || keyUUID(16) || be64(uid)) — 92 bytes.
func TestBoxWrapRoundTrip(t *testing.T) {
	priv, pub, err := newKeypair()
	if err != nil {
		t.Fatal(err)
	}
	var keyUUID [keyUUIDSize]byte
	copy(keyUUID[:], "0123456789abcdef")
	fk := bytes.Repeat([]byte{0x42}, fileKeySize)

	blob, err := boxWrap(pub, fk, keyUUID, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) != boxWrapSize {
		t.Fatalf("box wrap = %d bytes, want %d", len(blob), boxWrapSize)
	}
	got, err := boxOpen(priv, blob, keyUUID, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fk) {
		t.Fatal("round-trip changed the file key")
	}

	// A second wrap of the same FK differs (fresh ephemeral keypair + nonce)
	// yet opens to the same key.
	blob2, err := boxWrap(pub, fk, keyUUID, 9)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(blob, blob2) {
		t.Fatal("two box wraps are byte-identical (ephemeral reuse?)")
	}

	// Negatives: wrong key UUID in the AD, wrong user ID in the AD, a
	// tampered byte, and the wrong recipient key all fail.
	var otherUUID [keyUUIDSize]byte
	copy(otherUUID[:], "fedcba9876543210")
	if _, err := boxOpen(priv, blob, otherUUID, 9); err == nil {
		t.Error("wrong key uuid opened the box (AD not bound)")
	}
	if _, err := boxOpen(priv, blob, keyUUID, 10); err == nil {
		t.Error("wrong user id opened the box (AD not bound)")
	}
	tampered := bytes.Clone(blob)
	tampered[boxWrapSize-1] ^= 0x01
	if _, err := boxOpen(priv, tampered, keyUUID, 9); err == nil {
		t.Error("tampered box opened")
	}
	wrongPriv, _, err := newKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := boxOpen(wrongPriv, blob, keyUUID, 9); err == nil {
		t.Error("wrong recipient key opened the box")
	}
	if _, err := boxOpen(priv, blob[:boxWrapSize-4], keyUUID, 9); err == nil {
		t.Error("truncated box opened")
	}
}

// TestKDFMarshalParse pins the pw_kdf TEXT form: "m=<KB>,t=<it>,p=<par>".
func TestKDFMarshalParse(t *testing.T) {
	p := KeyDerivationParams{MemoryKB: 65536, Iterations: 3, Parallelism: 4}
	if got := p.marshalKDF(); got != "m=65536,t=3,p=4" {
		t.Fatalf("marshal = %q", got)
	}
	got, err := parseKDF(p.marshalKDF())
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("round-trip = %+v, want %+v", got, p)
	}
	for _, bad := range []string{
		"",
		"m=65536,t=3",          // missing field
		"m=65536,t=3,p=4,x=1",  // trailing garbage
		"m=65536,p=4,t=3",      // wrong order (pinned form is exact)
		"m=0,t=3,p=4",          // zero memory
		"m=65536,t=0,p=4",      // zero iterations
		"m=65536,t=3,p=0",      // zero parallelism
		"m=65536,t=3,p=256",    // parallelism out of uint8 range
		"m=abc,t=3,p=4",        // non-numeric
		"v=19,m=65536,t=3,p=4", // unknown leading field
		"65536,3,4",            // no field names
	} {
		if _, err := parseKDF(bad); err == nil {
			t.Errorf("parseKDF(%q) succeeded, want error", bad)
		}
	}
	// The pinned error names the malformed input.
	if _, err := parseKDF("junk"); err == nil || !strings.Contains(err.Error(), "junk") {
		t.Errorf("malformed error = %v, want it to name the input", err)
	}
}

// TestSessionCopyRoundTrip pins the sessions.sealed_uk construction:
// keyID(1) || nonce(12) || AES-256-GCM(ring[keyID], priv, "NCGOSK1" ||
// session-id) — including the key-ID byte surviving a ring extension
// (ADR-0101's refinement of ADR-0100 §3).
func TestSessionCopyRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	keyA, keyB := testKey(t), testKey(t)
	priv, _, err := newKeypair()
	if err != nil {
		t.Fatal(err)
	}
	resA, err := NewSQLResolver(db, [][]byte{keyA})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := resA.SealSessionKey(priv, "session-id-hex")
	if err != nil {
		t.Fatal(err)
	}
	if sealed[0] != 0 {
		t.Fatalf("key id byte = %d, want 0 (single-key ring)", sealed[0])
	}
	if len(sealed) != 1+pwSealedUKSize {
		t.Fatalf("session copy = %d bytes, want %d", len(sealed), 1+pwSealedUKSize)
	}
	got, err := resA.UnsealSessionKey(ctx, "session-id-hex", sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv) {
		t.Fatal("round-trip changed the private key")
	}

	// Ring extended by a rotation (A retires to position 0, B is current):
	// the recorded key-ID byte still selects key A. New copies seal under B.
	resAB, err := NewSQLResolver(db, [][]byte{keyA, keyB})
	if err != nil {
		t.Fatal(err)
	}
	got, err = resAB.UnsealSessionKey(ctx, "session-id-hex", sealed)
	if err != nil {
		t.Fatalf("unseal with the extended ring: %v", err)
	}
	if !bytes.Equal(got, priv) {
		t.Fatal("extended-ring unseal changed the private key")
	}
	sealedB, err := resAB.SealSessionKey(priv, "session-id-hex")
	if err != nil {
		t.Fatal(err)
	}
	if sealedB[0] != 1 {
		t.Fatalf("post-rotation key id byte = %d, want 1 (current)", sealedB[0])
	}

	// Negatives: wrong session id (AD), a tampered byte, and a key-ID byte
	// beyond the ring all fail with ErrIntegrity — never silently.
	if _, err := resA.UnsealSessionKey(ctx, "other-session", sealed); !errors.Is(err, ErrIntegrity) {
		t.Errorf("wrong session id err = %v, want ErrIntegrity", err)
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := resA.UnsealSessionKey(ctx, "session-id-hex", tampered); !errors.Is(err, ErrIntegrity) {
		t.Errorf("tampered copy err = %v, want ErrIntegrity", err)
	}
	outOfRing := bytes.Clone(sealed)
	outOfRing[0] = 9
	if _, err := resA.UnsealSessionKey(ctx, "session-id-hex", outOfRing); !errors.Is(err, ErrIntegrity) {
		t.Errorf("out-of-ring key id err = %v, want ErrIntegrity", err)
	}
	if _, err := resA.UnsealSessionKey(ctx, "session-id-hex", sealed[:5]); !errors.Is(err, ErrIntegrity) {
		t.Errorf("truncated copy err = %v, want ErrIntegrity", err)
	}
}
