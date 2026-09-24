package preview

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// countingStorage wraps the cache storage and records Create calls, so
// idempotency tests can prove a second run wrote nothing new.
type countingStorage struct {
	storage.Storage
	creates int
}

func (c *countingStorage) Create(ctx context.Context, p string, size int64) (io.WriteCloser, error) {
	c.creates++
	return c.Storage.Create(ctx, p, size)
}

// captureUploads wires a bus onto the DAV and returns a getter for the
// verbatim files.uploaded payloads it published — the real msgpack bytes the
// job consumes in production.
func captureUploads(t *testing.T, dav *files.DAV) func() [][]byte {
	t.Helper()
	var mu sync.Mutex
	var payloads [][]byte
	bus := events.NewBus(slog.New(slog.DiscardHandler))
	dav.Events = bus
	bus.Subscribe(func(_ context.Context, ev events.Event) {
		if ev.Topic != files.EventFilesUploaded {
			return
		}
		mu.Lock()
		payloads = append(payloads, ev.Payload)
		mu.Unlock()
	})
	t.Cleanup(func() { dav.Events = nil })
	return func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), payloads...)
	}
}

func uploadEntry(t *testing.T, dav *files.DAV, path string, data []byte) *webdav.Entry {
	t.Helper()
	ent, _, err := dav.Write(context.Background(), "alice", path, bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("upload %s: %v", path, err)
	}
	return ent
}

func cacheEntries(t *testing.T, st storage.Storage) []*storage.FileInfo {
	t.Helper()
	infos, err := st.List(context.Background(), "appdata_ocTestInstance/previews")
	if err != nil {
		return nil
	}
	return infos
}

func cachedPreview(t *testing.T, st storage.Storage, path, etag string, box int, ext string) []byte {
	t.Helper()
	key := cacheKey("alice", path, etag, box, box, false)
	f, err := st.Open(context.Background(), "appdata_ocTestInstance/previews/"+key+ext)
	if err != nil {
		t.Fatalf("cache entry for %dx%d%s: %v", box, box, ext, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPregeneratePNGAndJPEG(t *testing.T) {
	dav, st := newTestDAV(t)
	payloads := captureUploads(t, dav)
	pngEnt := uploadEntry(t, dav, "/photo.png", makePNG(t, 800, 600))
	jpgEnt := uploadEntry(t, dav, "/photo.jpg", makeJPEG(t, 400, 300))
	gen := newTestGenerator(t, dav, st, 0)
	job := NewPregenerateJob(gen, []int{32, 256}, slog.New(slog.DiscardHandler))

	got := payloads()
	if len(got) != 2 {
		t.Fatalf("captured %d payloads, want 2", len(got))
	}
	for _, p := range got {
		if err := job.Run(context.Background(), p); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	entries := cacheEntries(t, st)
	if len(entries) != 4 {
		t.Fatalf("cache entries = %d, want 4 (2 files x 2 boxes)", len(entries))
	}

	// PNG source: 800x600 fits to 32x24 and 256x192, encoded as .png.
	if w, h := decodeDims(t, cachedPreview(t, st, "/photo.png", pngEnt.ETag, 32, extPNG)); w != 32 || h != 24 {
		t.Errorf("png 32-box dims = %dx%d, want 32x24", w, h)
	}
	if w, h := decodeDims(t, cachedPreview(t, st, "/photo.png", pngEnt.ETag, 256, extPNG)); w != 256 || h != 192 {
		t.Errorf("png 256-box dims = %dx%d, want 256x192", w, h)
	}
	// JPEG source: stays .jpg.
	if w, h := decodeDims(t, cachedPreview(t, st, "/photo.jpg", jpgEnt.ETag, 32, extJPEG)); w != 32 || h != 24 {
		t.Errorf("jpg 32-box dims = %dx%d, want 32x24", w, h)
	}
	if w, h := decodeDims(t, cachedPreview(t, st, "/photo.jpg", jpgEnt.ETag, 256, extJPEG)); w != 256 || h != 192 {
		t.Errorf("jpg 256-box dims = %dx%d, want 256x192", w, h)
	}
	// The opposite extension must not exist for either file.
	pngWrong := cacheKey("alice", "/photo.png", pngEnt.ETag, 32, 32, false)
	if _, err := st.Stat(context.Background(), "appdata_ocTestInstance/previews/"+pngWrong+extJPEG); err == nil {
		t.Error("png source got a .jpg cache entry")
	}
	jpgWrong := cacheKey("alice", "/photo.jpg", jpgEnt.ETag, 32, 32, false)
	if _, err := st.Stat(context.Background(), "appdata_ocTestInstance/previews/"+jpgWrong+extPNG); err == nil {
		t.Error("jpeg source got a .png cache entry")
	}
}

func TestPregenerateNotPreviewable(t *testing.T) {
	dav, st := newTestDAV(t)
	payloads := captureUploads(t, dav)
	uploadEntry(t, dav, "/notes.txt", []byte("hello world, this is plain text"))
	garbage := make([]byte, 4096)
	for i := range garbage {
		garbage[i] = byte(i * 31)
	}
	uploadEntry(t, dav, "/e2ee.blob", garbage)
	// Corrupt image: valid PNG magic (passes the head-sniff), no valid IHDR.
	corrupt := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0x00}, 100)...)
	uploadEntry(t, dav, "/corrupt.png", corrupt)
	gen := newTestGenerator(t, dav, st, 0)
	job := NewPregenerateJob(gen, []int{32, 256}, slog.New(slog.DiscardHandler))

	for _, p := range payloads() {
		if err := job.Run(context.Background(), p); err != nil {
			t.Fatalf("Run: %v (per-file conditions must not be retryable)", err)
		}
	}
	if entries := cacheEntries(t, st); len(entries) != 0 {
		t.Errorf("cache entries = %d, want 0 for non-previewable uploads", len(entries))
	}
}

func TestPregenerateMissingFile(t *testing.T) {
	dav, st := newTestDAV(t)
	gen := newTestGenerator(t, dav, st, 0)
	job := NewPregenerateJob(gen, []int{32}, slog.New(slog.DiscardHandler))

	payload, err := msgpack.Marshal(map[string]any{"user": "alice", "path": "/never-uploaded.png", "size": 12, "created": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Run(context.Background(), payload); err != nil {
		t.Fatalf("Run: %v (deleted-before-run must not be retryable)", err)
	}
	if entries := cacheEntries(t, st); len(entries) != 0 {
		t.Errorf("cache entries = %d, want 0", len(entries))
	}
}

func TestPregenerateIdempotent(t *testing.T) {
	dav, st := newTestDAV(t)
	payloads := captureUploads(t, dav)
	uploadEntry(t, dav, "/photo.png", makePNG(t, 800, 600))
	cs := &countingStorage{Storage: st}
	gen := newTestGenerator(t, dav, cs, 0)
	job := NewPregenerateJob(gen, []int{32, 256}, slog.New(slog.DiscardHandler))

	p := payloads()[0]
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if cs.creates != 2 {
		t.Fatalf("first run created %d entries, want 2", cs.creates)
	}
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if cs.creates != 2 {
		t.Errorf("second run created %d more entries, want 0 (at-least-once must be idempotent)", cs.creates-2)
	}
}

func TestPregenerateClampDedupe(t *testing.T) {
	dav, st := newTestDAV(t)
	payloads := captureUploads(t, dav)
	ent := uploadEntry(t, dav, "/photo.png", makePNG(t, 800, 600))
	gen := newTestGenerator(t, dav, st, 2048)
	job := NewPregenerateJob(gen, []int{32, 4096, 32}, slog.New(slog.DiscardHandler))

	if err := job.Run(context.Background(), payloads()[0]); err != nil {
		t.Fatal(err)
	}
	entries := cacheEntries(t, st)
	if len(entries) != 2 {
		t.Fatalf("cache entries = %d, want 2 (4096 clamps to MaxDim 2048, duplicate 32 deduped)", len(entries))
	}
	// 32-box scales down; the clamped 2048 box never upscales the 800x600 source.
	if w, h := decodeDims(t, cachedPreview(t, st, "/photo.png", ent.ETag, 32, extPNG)); w != 32 || h != 24 {
		t.Errorf("32-box dims = %dx%d, want 32x24", w, h)
	}
	if w, h := decodeDims(t, cachedPreview(t, st, "/photo.png", ent.ETag, 2048, extPNG)); w != 800 || h != 600 {
		t.Errorf("clamped 2048-box dims = %dx%d, want 800x600 (never upscale)", w, h)
	}
	key4096 := cacheKey("alice", "/photo.png", ent.ETag, 4096, 4096, false)
	if _, err := st.Stat(context.Background(), "appdata_ocTestInstance/previews/"+key4096+extPNG); err == nil {
		t.Error("an unclamped 4096 entry exists")
	}
}

func TestPregenerateBadPayload(t *testing.T) {
	dav, st := newTestDAV(t)
	gen := newTestGenerator(t, dav, st, 0)
	job := NewPregenerateJob(gen, []int{32}, slog.New(slog.DiscardHandler))

	mk := func(v any) []byte {
		p, err := msgpack.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	payloads := [][]byte{
		{0xc1, 0xc1, 0xc1},                         // msgpack "never used" byte
		mk("just a string"),                        // valid msgpack, wrong shape
		mk(map[string]any{"size": 42}),             // user/path missing
		mk(map[string]any{"user": "", "path": ""}), // present but empty
		mk(map[string]any{"user": 42, "path": 42}), // wrong types
	}
	for i, p := range payloads {
		if err := job.Run(context.Background(), p); err != nil {
			t.Errorf("payload %d: Run = %v, want nil (bad payloads are not retryable)", i, err)
		}
	}
	if entries := cacheEntries(t, st); len(entries) != 0 {
		t.Errorf("cache entries = %d, want 0", len(entries))
	}
}

func TestPregenerateJobNilAndEmpty(t *testing.T) {
	payload, err := msgpack.Marshal(map[string]any{"user": "alice", "path": "/photo.png"})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPregenerateJob(nil, []int{32}, nil).Run(context.Background(), payload); err != nil {
		t.Errorf("nil gen: Run = %v", err)
	}
	dav, st := newTestDAV(t)
	gen := newTestGenerator(t, dav, st, 0)
	if err := NewPregenerateJob(gen, nil, nil).Run(context.Background(), payload); err != nil {
		t.Errorf("empty boxes: Run = %v", err)
	}
	if NewPregenerateJob(gen, []int{32}, nil).Name() != jobs.JobPreviewPregenerate {
		t.Errorf("Name = %q", NewPregenerateJob(gen, []int{32}, nil).Name())
	}
}
