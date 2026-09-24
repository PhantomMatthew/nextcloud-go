package preview

import (
	"context"
	"net/http"
	"os"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPreviewWebPLossy(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.webp", loadFixture(t, "lossy.webp"))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/photo.webp&x=64&y=64")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	// No WebP encoder exists in scope: output re-encodes as PNG.
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if w, h := decodeDims(t, rr.Body.Bytes()); w != 64 || h != 38 {
		t.Errorf("dims = %dx%d, want 64x38 (100x60 fit into 64)", w, h)
	}
	// The cache entry is the .png variant.
	infos, err := st.List(context.Background(), "appdata_ocTestInstance/previews")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(infos))
	}
}

func TestPreviewWebPLosslessAlpha(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/alpha.webp", loadFixture(t, "lossless-alpha.webp"))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/alpha.webp&x=40&y=40")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	img := decodeImage(t, rr.Body.Bytes())
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 40 || h != 25 {
		t.Fatalf("dims = %dx%d, want 40x25 (80x50 fit into 40)", w, h)
	}
	// Left half of the source is opaque red, right half fully transparent;
	// the PNG re-encode must preserve that alpha.
	if c := rgbaAt(t, img, 5, 5); c.A != 255 || c.R < 200 {
		t.Errorf("opaque quadrant = %+v, want opaque red", c)
	}
	if c := rgbaAt(t, img, 30, 12); c.A != 0 {
		t.Errorf("transparent quadrant alpha = %d, want 0", c.A)
	}
}

func TestPreviewWebPAnimated404(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/anim.webp", loadFixture(t, "animated.webp"))
	gen := newTestGenerator(t, dav, st, 0)

	// x/image/webp decodes still VP8/VP8L only; animations fail Decode and
	// collapse to the uniform 404, indistinguishable from a missing file.
	rr := getPreview(t, gen, "alice", "file=/anim.webp&x=32&y=32")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for animated webp", rr.Code)
	}
}

func TestPregenerateWebP(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.webp", loadFixture(t, "lossy.webp"))
	gen := newTestGenerator(t, dav, st, 0)

	// The head-sniff gate must let WebP through to the warm path.
	if err := gen.Pregenerate(context.Background(), "alice", "/photo.webp", []int{32}); err != nil {
		t.Fatalf("Pregenerate: %v", err)
	}
	infos, err := st.List(context.Background(), "appdata_ocTestInstance/previews")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("cache entries = %d, want 1 (webp prewarmed as png)", len(infos))
	}
}
