package preview

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// countingSource wraps the DAV and records how many source bytes the
// generator actually consumes, so cache-hit tests can prove the original
// was not read again.
type countingSource struct {
	inner     *files.DAV
	calls     int
	readBytes int64
}

func (c *countingSource) Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	c.calls++
	rc, e, err := c.inner.Read(ctx, user, p)
	if err != nil {
		return nil, nil, err
	}
	return &countingRC{ReadCloser: rc, owner: c}, e, nil
}

type countingRC struct {
	io.ReadCloser
	owner *countingSource
}

func (c *countingRC) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.owner.readBytes += int64(n)
	return n, err
}

func newTestDAV(t *testing.T) (*files.DAV, storage.Storage) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return files.NewDAV(st, files.NewSQLStore(db), us), st
}

func newTestGenerator(t *testing.T, src Source, st storage.Storage, maxDim int) *Generator {
	t.Helper()
	if maxDim == 0 {
		maxDim = 2048
	}
	return NewGenerator(src, st, "appdata_ocTestInstance/previews", maxDim, slog.New(slog.DiscardHandler))
}

func fillRGBA(img *image.RGBA) {
	b := img.Bounds()
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x % 256),
				G: uint8(y % 256),
				B: uint8((x * y) % 256),
				A: 255,
			})
		}
	}
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fillRGBA(img)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fillRGBA(img)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{
		color.RGBA{0, 0, 0, 255},
		color.RGBA{255, 0, 0, 255},
		color.RGBA{0, 255, 0, 255},
		color.RGBA{0, 0, 255, 255},
	}
	img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetColorIndex(x, y, uint8((x+y)%4))
		}
	}
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// hugePNGHeader is a syntactically valid PNG stream whose IHDR claims
// 50000x50000 pixels, with no pixel data behind it.
func hugePNGHeader() []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 50000)
	binary.BigEndian.PutUint32(ihdr[4:8], 50000)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // truecolor
	writeChunk(&buf, "IHDR", ihdr)
	writeChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writeChunk(buf *bytes.Buffer, typ string, data []byte) {
	_ = binary.Write(buf, binary.BigEndian, uint32(len(data)))
	buf.WriteString(typ)
	buf.Write(data)
	crc := crc32.ChecksumIEEE(append([]byte(typ), data...))
	_ = binary.Write(buf, binary.BigEndian, crc)
}

func upload(t *testing.T, dav *files.DAV, path string, data []byte) {
	t.Helper()
	if _, _, err := dav.Write(context.Background(), "alice", path, bytes.NewReader(data), nil); err != nil {
		t.Fatalf("upload %s: %v", path, err)
	}
}

func getPreview(t *testing.T, gen *Generator, uid, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/index.php/core/preview?"+rawQuery, nil)
	if uid != "" {
		req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: uid, Enabled: true}))
	}
	rr := httptest.NewRecorder()
	gen.ServeHTTP(rr, req)
	return rr
}

func decodeDims(t *testing.T, body []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not a decodable image: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestPreviewPNG(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 800, 600))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/photo.png&x=100&y=100")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "private, max-age=86400" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if et := rr.Header().Get("ETag"); et == "" {
		t.Error("ETag header missing")
	}
	w, h := decodeDims(t, rr.Body.Bytes())
	if w != 100 || h != 75 {
		t.Errorf("dims = %dx%d, want 100x75 (aspect-preserving fit)", w, h)
	}
}

func TestPreviewJPEG(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.jpg", makeJPEG(t, 400, 300))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/photo.jpg&x=100&y=100")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg (jpeg sources stay jpeg)", ct)
	}
	w, h := decodeDims(t, rr.Body.Bytes())
	if w != 100 || h != 75 {
		t.Errorf("dims = %dx%d, want 100x75", w, h)
	}
}

func TestPreviewGIFBecomesPNG(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/anim.gif", makeGIF(t, 200, 100))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/anim.gif&x=50&y=50")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png (gif first frame -> png)", ct)
	}
	w, h := decodeDims(t, rr.Body.Bytes())
	if w != 50 || h != 25 {
		t.Errorf("dims = %dx%d, want 50x25", w, h)
	}
}

func TestPreviewDefaultBox(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 800, 600))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/photo.png")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	w, h := decodeDims(t, rr.Body.Bytes())
	if w != 32 || h != 24 {
		t.Errorf("dims = %dx%d, want 32x24 (default box)", w, h)
	}
}

func TestPreviewNoUpscale(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.jpg", makeJPEG(t, 400, 300))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/photo.jpg&x=2000&y=2000")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	w, h := decodeDims(t, rr.Body.Bytes())
	if w != 400 || h != 300 {
		t.Errorf("dims = %dx%d, want 400x300 (never upscale)", w, h)
	}
}

func TestPreviewClampsToMaxDim(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 800, 600))
	gen := newTestGenerator(t, dav, st, 100)

	rr := getPreview(t, gen, "alice", "file=/photo.png&x=99999&y=99999")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (over-max dims clamp, not reject)", rr.Code)
	}
	w, h := decodeDims(t, rr.Body.Bytes())
	if w != 100 || h != 75 {
		t.Errorf("dims = %dx%d, want 100x75 (clamped to MaxDim)", w, h)
	}
}

func TestPreviewCacheHit(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 800, 600))
	src := &countingSource{inner: dav}
	gen := newTestGenerator(t, src, st, 0)

	first := getPreview(t, gen, "alice", "file=/photo.png&x=100&y=100")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	readAfterFirst := src.readBytes
	if readAfterFirst == 0 {
		t.Fatal("generator never read the source")
	}

	infos, err := st.List(context.Background(), "appdata_ocTestInstance/previews")
	if err != nil || len(infos) != 1 {
		t.Fatalf("cache listing = %v, %v; want exactly 1 entry", infos, err)
	}

	second := getPreview(t, gen, "alice", "file=/photo.png&x=100&y=100")
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d", second.Code)
	}
	if src.readBytes != readAfterFirst {
		t.Errorf("cache hit re-read %d source bytes", src.readBytes-readAfterFirst)
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Error("cached response body differs from generated one")
	}
	if ct := second.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("cached Content-Type = %q", ct)
	}
}

func TestPreviewETagInvalidation(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 800, 600))
	src := &countingSource{inner: dav}
	gen := newTestGenerator(t, src, st, 0)

	if rr := getPreview(t, gen, "alice", "file=/photo.png&x=100&y=100"); rr.Code != http.StatusOK {
		t.Fatalf("first status = %d", rr.Code)
	}
	readAfterFirst := src.readBytes

	upload(t, dav, "/photo.png", makePNG(t, 640, 480))
	rr := getPreview(t, gen, "alice", "file=/photo.png&x=100&y=100")
	if rr.Code != http.StatusOK {
		t.Fatalf("status after rewrite = %d", rr.Code)
	}
	if src.readBytes == readAfterFirst {
		t.Error("rewrite (new etag) must invalidate the cached preview")
	}
	w, h := decodeDims(t, rr.Body.Bytes())
	if w != 100 || h != 75 {
		t.Errorf("dims = %dx%d, want 100x75 from the new 640x480 source", w, h)
	}
}

func TestPreviewBadParams(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 64, 64))
	gen := newTestGenerator(t, dav, st, 0)

	for _, q := range []string{
		"",
		"x=100&y=100",
		"file=/photo.png&x=0",
		"file=/photo.png&x=-5",
		"file=/photo.png&x=abc",
		"file=/photo.png&y=0",
		"file=/photo.png&y=-1",
		"file=/photo.png&y=1.5",
		"file=/&x=10&y=10",
		"file=/../secret&x=10&y=10",
		"file=/a/../../b&x=10&y=10",
		"file=..&x=10&y=10",
	} {
		rr := getPreview(t, gen, "alice", q)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400", q, rr.Code)
		}
	}
}

func TestPreviewNotFound(t *testing.T) {
	dav, st := newTestDAV(t)
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/nope.png&x=10&y=10")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestPreviewNonImage(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/notes.txt", []byte("hello world, this is plain text"))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/notes.txt&x=10&y=10")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no fallback to source bytes)", rr.Code)
	}
}

func TestPreviewEncryptedGarbage(t *testing.T) {
	dav, st := newTestDAV(t)
	garbage := make([]byte, 4096)
	for i := range garbage {
		garbage[i] = byte(i * 31)
	}
	upload(t, dav, "/e2ee.blob", garbage)
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/e2ee.blob&x=10&y=10")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for opaque/encrypted blobs", rr.Code)
	}
}

func TestPreviewBombHeader(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/bomb.png", hugePNGHeader())
	src := &countingSource{inner: dav}
	gen := newTestGenerator(t, src, st, 0)

	rr := getPreview(t, gen, "alice", "file=/bomb.png&x=100&y=100")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for over-limit DecodeConfig dimensions", rr.Code)
	}
}

func TestPreviewForeignUser(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 64, 64))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "mallory", "file=/photo.png&x=10&y=10")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown/foreign user", rr.Code)
	}
}

func TestPreviewUnauthenticated(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 64, 64))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "", "file=/photo.png&x=10&y=10")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}
