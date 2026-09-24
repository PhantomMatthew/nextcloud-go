package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"testing"
)

// makeQuadrantPNG builds a w×h PNG with distinct quadrant colours:
// TL red, TR green, BL blue, BR yellow.
func makeQuadrantPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var c color.RGBA
			switch {
			case x < w/2 && y < h/2:
				c = color.RGBA{R: 255, A: 255}
			case x >= w/2 && y < h/2:
				c = color.RGBA{G: 200, A: 255}
			case x < w/2:
				c = color.RGBA{B: 255, A: 255}
			default:
				c = color.RGBA{R: 255, G: 200, A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeImage(t *testing.T, body []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not a decodable image: %v", err)
	}
	return img
}

func rgbaAt(t *testing.T, img image.Image, x, y int) color.RGBA {
	t.Helper()
	c, ok := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	if !ok {
		t.Fatalf("pixel %d,%d not RGBA", x, y)
	}
	return c
}

func TestPreviewFillCentreCrop(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/quad.png", makeQuadrantPNG(t, 800, 600))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/quad.png&x=256&y=256&mode=fill")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	img := decodeImage(t, rr.Body.Bytes())
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 256 || h != 256 {
		t.Fatalf("fill dims = %dx%d, want exactly 256x256", w, h)
	}
	// 800x600 cover-scaled to 341x256 then centre-cropped to 256x256 keeps
	// original x∈[~98,~698]: the output's left edge is still in the red/blue
	// half, its right edge in the green/yellow half.
	if c := rgbaAt(t, img, 8, 8); c.R < 200 || c.G > 80 || c.B > 80 {
		t.Errorf("top-left = %+v, want red-dominant", c)
	}
	if c := rgbaAt(t, img, 247, 8); c.G < 150 || c.R > 80 || c.B > 80 {
		t.Errorf("top-right = %+v, want green-dominant", c)
	}
	if c := rgbaAt(t, img, 8, 247); c.B < 200 || c.R > 80 || c.G > 80 {
		t.Errorf("bottom-left = %+v, want blue-dominant", c)
	}
	if c := rgbaAt(t, img, 247, 247); c.R < 200 || c.G < 150 || c.B > 80 {
		t.Errorf("bottom-right = %+v, want yellow-dominant", c)
	}
}

func TestPreviewFillNeverUpscales(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/small.png", makePNG(t, 100, 50))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/small.png&x=256&y=256&mode=fill")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	if w, h := decodeDims(t, rr.Body.Bytes()); w != 100 || h != 50 {
		t.Errorf("fill dims = %dx%d, want 100x50 (never upscale)", w, h)
	}
}

func TestPreviewFillPartialCrop(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 800, 600))
	gen := newTestGenerator(t, dav, st, 0)

	// Box wider than the never-upscale source: cover keeps 800x600, then
	// the height centre-crops to the box.
	rr := getPreview(t, gen, "alice", "file=/photo.png&x=1024&y=256&mode=fill")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	if w, h := decodeDims(t, rr.Body.Bytes()); w != 800 || h != 256 {
		t.Errorf("fill dims = %dx%d, want 800x256", w, h)
	}
}

func TestPreviewModeFallbackToFit(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 800, 600))
	gen := newTestGenerator(t, dav, st, 0)

	var wantETag string
	for i, mode := range []string{"", "mode=crop", "mode=bogus"} {
		q := "file=/photo.png&x=256&y=256"
		if mode != "" {
			q += "&" + mode
		}
		rr := getPreview(t, gen, "alice", q)
		if rr.Code != http.StatusOK {
			t.Fatalf("mode %q: status = %d", mode, rr.Code)
		}
		if w, h := decodeDims(t, rr.Body.Bytes()); w != 256 || h != 192 {
			t.Errorf("mode %q: dims = %dx%d, want 256x192 (fit)", mode, w, h)
		}
		et := rr.Header().Get("ETag")
		if i == 0 {
			wantETag = et
		} else if et != wantETag {
			t.Errorf("mode %q: ETag %s != fit ETag %s (should share the cache entry)", mode, et, wantETag)
		}
	}
}

func TestPreviewFillCacheCoexistence(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.png", makePNG(t, 800, 600))
	cs := &countingSource{inner: dav}
	gen := newTestGenerator(t, cs, st, 0)

	fit := getPreview(t, gen, "alice", "file=/photo.png&x=256&y=256")
	fill := getPreview(t, gen, "alice", "file=/photo.png&x=256&y=256&mode=fill")
	if fit.Code != http.StatusOK || fill.Code != http.StatusOK {
		t.Fatalf("fit %d, fill %d", fit.Code, fill.Code)
	}
	if fit.Header().Get("ETag") == fill.Header().Get("ETag") {
		t.Error("fit and fill share an ETag — mode is not part of the cache key")
	}
	infos, err := st.List(context.Background(), "appdata_ocTestInstance/previews")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("cache entries = %d, want 2 (fit and fill coexist)", len(infos))
	}

	// A second fill request must be a pure cache hit: no further source
	// bytes consumed.
	before := cs.readBytes
	again := getPreview(t, gen, "alice", "file=/photo.png&x=256&y=256&mode=fill")
	if again.Code != http.StatusOK {
		t.Fatalf("second fill: status = %d", again.Code)
	}
	if cs.readBytes != before {
		t.Errorf("cache hit consumed %d source bytes", cs.readBytes-before)
	}
	if !bytes.Equal(again.Body.Bytes(), fill.Body.Bytes()) {
		t.Error("cache-hit body differs from generated body")
	}
}

func TestPreviewFillJPEG(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/photo.jpg", makeJPEG(t, 800, 600))
	gen := newTestGenerator(t, dav, st, 0)

	rr := getPreview(t, gen, "alice", "file=/photo.jpg&x=200&y=200&mode=fill")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if w, h := decodeDims(t, rr.Body.Bytes()); w != 200 || h != 200 {
		t.Errorf("dims = %dx%d, want 200x200", w, h)
	}
}

// TestCacheKeyFitCompat pins the v1 fit key format byte-for-byte (existing
// cache entries stay valid) and the fill marker separation.
func TestCacheKeyFitCompat(t *testing.T) {
	want := sha256.Sum256([]byte("alice\n/photo.png\netag123\n256x256"))
	if got := cacheKey("alice", "/photo.png", "etag123", 256, 256, false); got != hex.EncodeToString(want[:]) {
		t.Errorf("fit key changed format: %s", got)
	}
	fit := cacheKey("alice", "/photo.png", "etag123", 256, 256, false)
	fill := cacheKey("alice", "/photo.png", "etag123", 256, 256, true)
	if fit == fill {
		t.Error("fit and fill keys collide")
	}
	wantFill := sha256.Sum256([]byte("alice\n/photo.png\netag123\n256x256\nfill"))
	if fill != hex.EncodeToString(wantFill[:]) {
		t.Errorf("fill key = %s, want sha256 of the \\nfill-suffixed string", fill)
	}
}
