package encrypt

import (
	"bytes"
	"context"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
)

// TestOwnableStorageKey pins the ADR-0105 §1 truth table: uid trees and the
// uploads/versions/trash <uid>-nested namespaces are ownable; the
// appdata_<instanceID> system trees and empty/rootless keys are not.
func TestOwnableStorageKey(t *testing.T) {
	cases := map[string]bool{
		"alice/docs/report.txt":          true,
		"alice":                          true,
		"uploads/alice/123/chunk":        true,
		"versions/alice/45":              true,
		"trash/alice/7":                  true,
		"appdata_ocTest/previews/ab.jpg": false,
		"appdata_ocTest/plugins/p1/f":    false,
		"appdata_":                       false,
		"appdata_ocTest":                 false,
		"uploads/":                       false,
		"versions//45":                   false,
		"uploads/appdata_ocTest/x":       false,
		"":                               false,
		".":                              false,
		"..":                             false,
	}
	for key, want := range cases {
		if got := OwnableStorageKey(key); got != want {
			t.Errorf("OwnableStorageKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestCreateAppdataV2FallbackByteExact pins ADR-0105 §1: with a resolver
// configured, Create on an ownerless (appdata_*) key seals v2 under the
// current keyring key, construction-identical to the same write on a
// resolver-less multi-key ring — and the resolver's Allocate is never
// consulted. User keys still seal v3.
func TestCreateAppdataV2FallbackByteExact(t *testing.T) {
	current, previous := testKey(t), testKey(t)

	innerRes, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := newStubResolver()
	withRes, err := NewWithResolver(current, [][]byte{previous}, innerRes, res)
	if err != nil {
		t.Fatal(err)
	}
	innerPlain, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	noRes, err := NewWithPrevious(current, [][]byte{previous}, innerPlain)
	if err != nil {
		t.Fatal(err)
	}

	const p = "appdata_ocTest/previews/deadbeef.jpg"
	data := bytes.Repeat([]byte{0x42}, 5000)
	writeAll(t, withRes, p, data)
	writeAll(t, noRes, p, data)

	rawRes := rawBytes(t, innerRes, p)
	rawPlain := rawBytes(t, innerPlain, p)
	// Header construction is byte-exact: NCGOENC2 magic + current key-ID
	// byte (the salt and sealed chunks are random by design).
	if !bytes.Equal(rawRes[:headerSizeV2-saltSize], rawPlain[:headerSizeV2-saltSize]) {
		t.Fatalf("appdata v2 header prefix differs: %q vs %q", rawRes[:headerSizeV2-saltSize], rawPlain[:headerSizeV2-saltSize])
	}
	if len(rawRes) != len(rawPlain) {
		t.Fatalf("stored sizes differ: %d vs %d", len(rawRes), len(rawPlain))
	}
	if string(rawRes[:len(magicV2)]) != magicV2 || rawRes[len(magicV2)] != 1 {
		t.Fatalf("appdata blob is not v2 under key ID 1: %q id=%d", rawRes[:len(magicV2)], rawRes[len(magicV2)])
	}
	if len(res.allocKey) != 0 {
		t.Fatalf("resolver Allocate called for ownerless keys: %v", res.allocKey)
	}
	// Reads auto-detect v2 on both FSes, resolver or not.
	if got := readAll(t, withRes, p); !bytes.Equal(got, data) {
		t.Errorf("resolver FS round trip = %d bytes, want %d", len(got), len(data))
	}
	if got := readAll(t, noRes, p); !bytes.Equal(got, data) {
		t.Errorf("resolver-less FS round trip = %d bytes, want %d", len(got), len(data))
	}

	// A user key still seals v3 through the same resolver-carrying FS.
	uuid := writeV3(t, withRes, "alice/docs/report.txt", []byte("v3 content"))
	raw := rawBytes(t, innerRes, "alice/docs/report.txt")
	if string(raw[:len(magicV3)]) != magicV3 {
		t.Fatal("user write lost its v3 envelope")
	}
	if !bytes.Equal(raw[len(magicV3):len(magicV3)+keyUUIDSize], uuid[:]) {
		t.Error("v3 header key uuid mismatch")
	}
}

// TestCreateAppdataSingleKeyV1 pins the single-key ring case: the fallback
// branch writes the v1 header bit-identically to pre-keyring deployments,
// exactly as resolver-less single-key writes do.
func TestCreateAppdataSingleKeyV1(t *testing.T) {
	res := newStubResolver()
	fs, inner := resolverFS(t, res)
	const p = "appdata_ocTest/plugins/p1/state.bin"
	writeAll(t, fs, p, []byte("plugin state"))
	raw := rawBytes(t, inner, p)
	if string(raw[:len(magic)]) != magic {
		t.Fatalf("appdata single-key write = %q, want v1 magic %q", raw[:len(magic)], magic)
	}
	if len(res.allocKey) != 0 {
		t.Fatalf("resolver Allocate called for ownerless keys: %v", res.allocKey)
	}
}

// TestSweepRotateCoversAppdata pins the rotate sweep's tree enumeration
// (ADR-0105 §1 consequence): the walk starts at the storage root, so
// appdata_<id>/ blobs sealed under a retired key are re-sealed like any
// other v1/v2 — through the resolver-carrying FS, whose Create falls back
// to v2 for the ownerless key (never Allocate).
func TestSweepRotateCoversAppdata(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyA, keyB := testKey(t), testKey(t)
	res := newStubResolver()
	fsA, err := NewWithResolver(keyA, nil, inner, res)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{
		"appdata_ocTest/previews/old.jpg": []byte("preview blob under the retiring key"),
		"appdata_ocTest/plugins/p/s.bin":  bytes.Repeat([]byte{0x7E}, 700),
		"alice/old.txt":                   []byte("user file under the retiring key"),
	}
	for p, data := range want {
		writeAll(t, fsA, p, data)
	}
	// The appdata writes fell back to v1 (single-key ring); the user write
	// is v3 and rotation must skip it (master-key rotation never reseals
	// per-user content keys, ADR-0096).
	if raw := rawBytes(t, inner, "alice/old.txt"); string(raw[:len(magicV3)]) != magicV3 {
		t.Fatal("user write is not v3-sealed")
	}
	ring, err := NewWithResolver(keyB, [][]byte{keyA}, inner, res)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := Sweep(ctx, inner, ring, SweepOptions{Direction: SweepRotate})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 3 || stats.Changed != 2 || stats.Skipped != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want scanned=3 changed=2 skipped=1 (v3 user file) failed=0", stats)
	}
	for p, data := range want {
		if got := readAll(t, ring, p); !bytes.Equal(got, data) {
			t.Errorf("%s: round trip = %d bytes, want %d", p, len(got), len(data))
		}
	}
	if !hasV2ID1(t, inner, "appdata_ocTest/previews/old.jpg") || !hasV2ID1(t, inner, "appdata_ocTest/plugins/p/s.bin") {
		t.Error("appdata blobs must be re-sealed as v2 under key ID 1")
	}
}
