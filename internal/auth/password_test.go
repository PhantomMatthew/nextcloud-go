package auth

import "testing"

func testParams() Argon2idParams {
	return Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16}
}

func TestArgon2idRoundTrip(t *testing.T) {
	h := NewArgon2id(testParams())
	hash, err := h.Hash("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := h.Verify(hash, "hunter2")
	if err != nil || !ok {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	ok, err = h.Verify(hash, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("wrong password accepted")
	}
}

func TestArgon2idTamperedHash(t *testing.T) {
	h := NewArgon2id(testParams())
	hash, err := h.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	tampered := hash[:len(hash)-2] + "xx"
	ok, err := h.Verify(tampered, "pw")
	if err == nil && ok {
		t.Fatal("tampered hash verified")
	}
}

func TestArgon2idInvalidHash(t *testing.T) {
	h := NewArgon2id(testParams())
	if _, err := h.Verify("not-a-hash", "pw"); err == nil {
		t.Fatal("expected ErrInvalidHash")
	}
}

func TestArgon2idNeedsRehash(t *testing.T) {
	h := NewArgon2id(testParams())
	hash, err := h.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	if h.NeedsRehash(hash) {
		t.Fatal("same params should not rehash")
	}
	stronger := NewArgon2id(Argon2idParams{MemoryKB: 16, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
	if !stronger.NeedsRehash(hash) {
		t.Fatal("changed memory should rehash")
	}
}
