package mail

import (
	"bytes"
	"errors"
	"testing"
)

const (
	credTestSecret = "test-instance-secret-0123456789"
	credTestUID    = "alice"
	credTestHost   = "imap.example.com"
	credTestUser   = "alice@example.com"
)

func TestSealCredentialRoundTrip(t *testing.T) {
	t.Parallel()
	plain := []byte("s3cret-password")
	blob, err := SealCredential(credTestSecret, credTestUID, credTestHost, credTestUser, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, plain) {
		t.Fatal("sealed blob contains the plaintext")
	}
	if len(blob) != credentialSaltSize+credentialNonceSize+len(plain)+16 {
		t.Errorf("blob len = %d", len(blob))
	}
	// Randomized salt+nonce: two seals of the same plaintext differ.
	again, err := SealCredential(credTestSecret, credTestUID, credTestHost, credTestUser, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(blob, again) {
		t.Error("two seals produced identical blobs (salt/nonce not randomized)")
	}
	got, err := OpenCredential(credTestSecret, credTestUID, credTestHost, credTestUser, blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("round trip = %q", got)
	}
	// Empty plaintext round-trips too (a present-but-empty smtp half).
	emptyBlob, err := SealCredential(credTestSecret, credTestUID, credTestHost, credTestUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotEmpty, err := OpenCredential(credTestSecret, credTestUID, credTestHost, credTestUser, emptyBlob)
	if err != nil || len(gotEmpty) != 0 {
		t.Errorf("empty round trip = %q, %v", gotEmpty, err)
	}
}

func TestOpenCredentialTamper(t *testing.T) {
	t.Parallel()
	plain := []byte("s3cret-password")
	blob, err := SealCredential(credTestSecret, credTestUID, credTestHost, credTestUser, plain)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		secret, uid, host, user string
		blob                    []byte
	}{
		"wrong secret":    {secret: "other-instance-secret", uid: credTestUID, host: credTestHost, user: credTestUser, blob: blob},
		"wrong user id":   {secret: credTestSecret, uid: "bob", host: credTestHost, user: credTestUser, blob: blob},
		"wrong imap host": {secret: credTestSecret, uid: credTestUID, host: "imap.evil.com", user: credTestUser, blob: blob},
		"wrong imap user": {secret: credTestSecret, uid: credTestUID, host: credTestHost, user: "bob@example.com", blob: blob},
		"empty blob":      {secret: credTestSecret, uid: credTestUID, host: credTestHost, user: credTestUser, blob: nil},
		"short blob":      {secret: credTestSecret, uid: credTestUID, host: credTestHost, user: credTestUser, blob: blob[:credentialSaltSize+4]},
		"truncated ct":    {secret: credTestSecret, uid: credTestUID, host: credTestHost, user: credTestUser, blob: blob[:len(blob)-4]},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := OpenCredential(tc.secret, tc.uid, tc.host, tc.user, tc.blob); !errors.Is(err, ErrCredential) {
				t.Errorf("err = %v, want ErrCredential", err)
			}
		})
	}
	// Flipped ciphertext byte: GCM auth fails.
	flipped := bytes.Clone(blob)
	flipped[len(flipped)-1] ^= 0x01
	if _, err := OpenCredential(credTestSecret, credTestUID, credTestHost, credTestUser, flipped); !errors.Is(err, ErrCredential) {
		t.Errorf("flipped ct: err = %v, want ErrCredential", err)
	}
	// Flipped salt byte: wrong key at derive time.
	flippedSalt := bytes.Clone(blob)
	flippedSalt[0] ^= 0x01
	if _, err := OpenCredential(credTestSecret, credTestUID, credTestHost, credTestUser, flippedSalt); !errors.Is(err, ErrCredential) {
		t.Errorf("flipped salt: err = %v, want ErrCredential", err)
	}
}

func TestSealCredentialEmptySecret(t *testing.T) {
	t.Parallel()
	if _, err := SealCredential("", credTestUID, credTestHost, credTestUser, []byte("x")); err == nil {
		t.Fatal("seal with empty secret: want error")
	}
	if _, err := OpenCredential("", credTestUID, credTestHost, credTestUser, []byte("x")); err == nil {
		t.Fatal("open with empty secret: want error")
	}
}

func TestJoinSplitPasswords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ imap, smtp string }{
		{"pw-imap", "pw-smtp"},
		{"same", "same"},
		{"pw", ""},
		{"with spaces and pünctüation", "smtp"},
	} {
		packed, err := joinPasswords(tc.imap, tc.smtp)
		if err != nil {
			t.Fatal(err)
		}
		imap, smtp, err := splitPasswords(packed)
		if err != nil {
			t.Fatal(err)
		}
		if imap != tc.imap || smtp != tc.smtp {
			t.Errorf("round trip = %q %q, want %q %q", imap, smtp, tc.imap, tc.smtp)
		}
	}
	if _, _, err := splitPasswords(nil); !errors.Is(err, ErrCredential) {
		t.Errorf("empty packed: err = %v, want ErrCredential", err)
	}
	if _, _, err := splitPasswords([]byte{0, 10, 'x'}); !errors.Is(err, ErrCredential) {
		t.Errorf("truncated packed: err = %v, want ErrCredential", err)
	}
}
