package plugins

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	members := testMembers()
	sigRaw, err := SignMembers(members, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := VerifyMembers(sigRaw, members, []ed25519.PublicKey{pub})
	if err != nil {
		t.Fatal(err)
	}
	if keyID != KeyIDFromPublic(pub) {
		t.Fatalf("keyid = %q", keyID)
	}
}

func TestVerifyTamperedMember(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	members := testMembers()
	sigRaw, err := SignMembers(members, priv)
	if err != nil {
		t.Fatal(err)
	}
	members["pack.wasm"] = []byte{0xde, 0xad}
	if _, err := VerifyMembers(sigRaw, members, []ed25519.PublicKey{pub}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyUntrustedKey(t *testing.T) {
	_, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	members := testMembers()
	sigRaw, err := SignMembers(members, priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMembers(sigRaw, members, []ed25519.PublicKey{other}); !errors.Is(err, ErrSignatureUntrusted) {
		t.Fatalf("err = %v", err)
	}
	// No trusted keys at all is also untrusted.
	if _, err := VerifyMembers(sigRaw, members, nil); !errors.Is(err, ErrSignatureUntrusted) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	pub, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMembers([]byte("{nope"), testMembers(), []ed25519.PublicKey{pub}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v", err)
	}
	if _, err := VerifyMembers([]byte(`{"keyid":"x","signature":"!!"}`), testMembers(), []ed25519.PublicKey{pub}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestSignBadKey(t *testing.T) {
	if _, err := SignMembers(testMembers(), []byte("short")); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v", err)
	}
}
