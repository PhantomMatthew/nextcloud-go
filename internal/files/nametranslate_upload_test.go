package files_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// uploadRawDest reads the raw uploads row's destination — the phase-3b
// at-rest view.
func (e *nameE2EEnv) uploadRawDest(t *testing.T, transferID string) string {
	t.Helper()
	var dest string
	if err := e.db.QueryRow(context.Background(),
		`SELECT destination FROM uploads WHERE transfer_id = ?`, transferID).Scan(&dest); err != nil {
		t.Fatal(err)
	}
	return dest
}

func (e *nameE2EEnv) uploadRowCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := e.db.QueryRow(context.Background(), `SELECT COUNT(*) FROM uploads`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestNameCryptUploadDestinationTokenized pins the TranslatingUploadStore row
// shape (ADR-0104 §6, phase 3b): a scheme-1 user's session destination is the
// ciphertext path at rest (no plaintext window), Get returns the plaintext
// view, UpdateDest re-tokenizes, a missing parent fails LOUDLY (never a
// plaintext fallback row), and a scheme-0 user's row is bit-identical to the
// pre-feature form.
func TestNameCryptUploadDestinationTokenized(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice")
	env.mkdir(t, "/docs")

	if _, err := env.uploadFS.MkdirMeta(ctx, "alice", "/tid1", webdav.CollectionMeta{Destination: "/docs/big.bin", TotalLength: 11}); err != nil {
		t.Fatal(err)
	}
	dest := env.uploadRawDest(t, "tid1")
	docsRow := env.rawRow(t, "/docs")
	if !strings.HasPrefix(dest, docsRow.Path+"/") {
		t.Errorf("destination = %q, want a child of the /docs ciphertext %q", dest, docsRow.Path)
	}
	for _, leak := range []string{"docs", "big", "bin"} {
		if strings.Contains(dest, leak) {
			t.Errorf("destination %q leaks %q", dest, leak)
		}
	}
	// The store boundary speaks plaintext: Get decrypts, and MkdirMeta left
	// the caller's session in the plaintext view.
	sess, err := env.uploadFS.Sessions.Get(ctx, env.ids["alice"], "tid1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Destination != "/docs/big.bin" {
		t.Errorf("Get destination = %q, want plaintext /docs/big.bin", sess.Destination)
	}

	// UpdateDest re-tokenizes (a new basename → a fresh token under the same
	// parent DK; deterministic, so the assembly's dest comparison is stable).
	if err := env.uploadFS.Sessions.UpdateDest(ctx, env.ids["alice"], "tid1", "/docs/renamed.bin", 11); err != nil {
		t.Fatal(err)
	}
	dest2 := env.uploadRawDest(t, "tid1")
	if dest2 == dest || !strings.HasPrefix(dest2, docsRow.Path+"/") || strings.Contains(dest2, "renamed") {
		t.Errorf("re-tokenized destination = %q (was %q)", dest2, dest)
	}
	sess, err = env.uploadFS.Sessions.Get(ctx, env.ids["alice"], "tid1")
	if err != nil || sess.Destination != "/docs/renamed.bin" {
		t.Errorf("Get after UpdateDest = %q %v, want /docs/renamed.bin", sess.Destination, err)
	}

	// A destination whose PARENT does not exist fails loudly — no session row
	// lands, and certainly no plaintext fallback row.
	if _, err := env.uploadFS.MkdirMeta(ctx, "alice", "/tid2", webdav.CollectionMeta{Destination: "/nope/f.bin", TotalLength: 1}); !errors.Is(err, webdav.ErrNotFound) {
		t.Errorf("missing-parent mkdir = %v, want ErrNotFound (loud)", err)
	}
	if got := env.uploadRowCount(t); got != 1 {
		t.Errorf("upload rows after the loud failure = %d, want 1", got)
	}

	// Scheme 0 (bob): the row is bit-identical to the pre-feature form.
	if _, err := env.uploadFS.MkdirMeta(ctx, "bob", "/tid3", webdav.CollectionMeta{Destination: "/plain.bin", TotalLength: 1}); err != nil {
		t.Fatal(err)
	}
	if got := env.uploadRawDest(t, "tid3"); got != "/plain.bin" {
		t.Errorf("scheme-0 destination = %q, want verbatim /plain.bin", got)
	}
	// An unset destination (no Destination header) passes through verbatim.
	if _, err := env.uploadFS.Mkdir(ctx, "alice", "/tid4"); err != nil {
		t.Fatal(err)
	}
	if got := env.uploadRawDest(t, "tid4"); got != "" {
		t.Errorf("unset destination = %q, want empty verbatim", got)
	}
}

// TestNameCryptChunkedUploadAssembles is the phase-3b end-to-end pin: the
// full chunked-upload v2 flow (MkdirMeta with a destination → chunk PUTs →
// assembly) lands the file at the right plaintext destination with a
// ciphertext files row, and the assembly's session-destination comparison and
// write both run on the translating views (uploads.go unchanged).
func TestNameCryptChunkedUploadAssembles(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")
	env.mkdir(t, "/docs")

	if _, err := env.uploadFS.MkdirMeta(ctx, "alice", "/tid1", webdav.CollectionMeta{Destination: "/docs/big.bin", TotalLength: 11}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.uploadFS.Write(ctx, "alice", "/tid1/00001", strings.NewReader("hello "), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.uploadFS.Write(ctx, "alice", "/tid1/000002", strings.NewReader("world"), nil); err != nil {
		t.Fatal(err)
	}
	// The session carries the ciphertext destination for its whole lifetime.
	sessDest := env.uploadRawDest(t, "tid1")
	if strings.Contains(sessDest, "big") {
		t.Fatalf("session destination %q leaks plaintext", sessDest)
	}

	// Assembly with no explicit destination (the session's own) — the
	// translating Get hands uploads.go the plaintext dest it compares and
	// writes through.
	ent, created, err := env.uploadFS.Assemble(ctx, "alice", "tid1", "alice", "", true, nil, "", "")
	if err != nil || !created {
		t.Fatalf("assemble = %+v created=%v err=%v", ent, created, err)
	}
	if ent.Path != "/docs/big.bin" || ent.Size != 11 {
		t.Errorf("assembled entry = %+v, want /docs/big.bin size 11", ent)
	}
	// The files row is ciphertext and agrees with the session's destination.
	row := env.rawRow(t, "/docs/big.bin")
	if row.Path != sessDest {
		t.Errorf("files row path = %q, want the session's ciphertext destination %q", row.Path, sessDest)
	}
	if strings.Contains(row.Name, "big") || row.NameScheme != 1 {
		t.Errorf("assembled row = %+v, want a scheme-1 token", row)
	}
	// The session row is gone (assembly removes the staging).
	if got := env.uploadRowCount(t); got != 0 {
		t.Errorf("upload rows after assembly = %d, want 0", got)
	}
	// The content reads back through the plaintext DAV view.
	rc, _, err := env.dav.Read(ctx, "alice", "/docs/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "hello world" {
		t.Fatalf("read = %q %v", body, err)
	}

	// The MOVE-shaped assembly (an explicit destination differing from the
	// session's) re-tokenizes through UpdateDest mid-flight.
	if _, err := env.uploadFS.MkdirMeta(ctx, "alice", "/tid2", webdav.CollectionMeta{Destination: "/docs/a.bin", TotalLength: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.uploadFS.Write(ctx, "alice", "/tid2/00001", strings.NewReader("x"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.uploadFS.Assemble(ctx, "alice", "tid2", "alice", "/docs/b.bin", true, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := env.dav.Stat(ctx, "alice", "/docs/b.bin"); err != nil {
		t.Fatalf("moved assembly destination missing: %v", err)
	}
}

// TestNameCryptUploadScheme0Unchanged pins the bit-compat carve-out at the
// store boundary: with the wrapper wired but the user scheme 0, every
// operation is byte-identical to the raw store.
func TestNameCryptUploadScheme0Unchanged(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "carol")

	if _, err := env.uploadFS.MkdirMeta(ctx, "carol", "/tid1", webdav.CollectionMeta{Destination: "/dir/f.bin", TotalLength: 1}); err != nil {
		t.Fatal(err)
	}
	if got := env.uploadRawDest(t, "tid1"); got != "/dir/f.bin" {
		t.Errorf("scheme-0 destination = %q, want verbatim /dir/f.bin", got)
	}
	if err := env.uploadFS.Sessions.UpdateDest(ctx, env.ids["carol"], "tid1", "/other.bin", 1); err != nil {
		t.Fatal(err)
	}
	if got := env.uploadRawDest(t, "tid1"); got != "/other.bin" {
		t.Errorf("scheme-0 destination after UpdateDest = %q, want verbatim /other.bin", got)
	}
	sess, err := env.uploadFS.Sessions.Get(ctx, env.ids["carol"], "tid1")
	if err != nil || sess.Destination != "/other.bin" {
		t.Errorf("scheme-0 Get = %q %v", sess.Destination, err)
	}
}
