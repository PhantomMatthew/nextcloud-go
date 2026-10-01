package preview

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
)

// fakeRasterizer stubs the office seam: no external binary is ever invoked
// in tests — the pipeline integration is what gets pinned.
type fakeRasterizer struct {
	handles bool
	out     []byte
	err     error
	calls   int
}

func (f *fakeRasterizer) Handles(string) bool { return f.handles }

func (f *fakeRasterizer) Rasterize(_ context.Context, _ []byte) ([]byte, error) {
	f.calls++
	return f.out, f.err
}

func makeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeDocx(t *testing.T) []byte {
	t.Helper()
	return makeZip(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   "<w:document/>",
	})
}

func TestOfficeMime(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{"pdf", []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj"), mimePDF, true},
		{"docx", makeDocx(t), mimeOOXMLWord, true},
		{"xlsx", makeZip(t, map[string]string{"[Content_Types].xml": "<Types/>", "xl/workbook.xml": "<workbook/>"}), mimeOOXMLSheet, true},
		{"pptx", makeZip(t, map[string]string{"[Content_Types].xml": "<Types/>", "ppt/presentation.xml": "<p/>"}), mimeOOXMLSlide, true},
		{"ooxml-no-main-part", makeZip(t, map[string]string{"[Content_Types].xml": "<Types/>", "custom.xml": "<c/>"}), "", false},
		{"odt", makeZip(t, map[string]string{"mimetype": mimeODFText, "content.xml": "<c/>"}), mimeODFText, true},
		{"ods", makeZip(t, map[string]string{"mimetype": mimeODFSheet}), mimeODFSheet, true},
		{"odf-bogus-mimetype", makeZip(t, map[string]string{"mimetype": "application/zip"}), "", false},
		{"plain-zip", makeZip(t, map[string]string{"foo.txt": "hello"}), "", false},
		{"truncated-zip", makeDocx(t)[:64], "", false},
		{"not-a-document", []byte("plain text body"), "", false},
		{"image-not-office", makePNG(t, 8, 8), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, ok := officeMime(tc.data)
			if ok != tc.ok || mime != tc.want {
				t.Errorf("officeMime = %q, %v; want %q, %v", mime, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPreviewOfficeViaRasterizer(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/report.docx", makeDocx(t))
	gen := newTestGenerator(t, dav, st, 0)
	fake := &fakeRasterizer{handles: true, out: makePNG(t, 640, 480)}
	gen.Rasterizer = fake

	rr := getPreview(t, gen, "alice", "file=/report.docx&x=100&y=100")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
	if w, h := decodeDims(t, rr.Body.Bytes()); w != 100 || h != 75 {
		t.Errorf("dims = %dx%d, want 100x75", w, h)
	}
	if fake.calls != 1 {
		t.Fatalf("rasterizer calls = %d, want 1", fake.calls)
	}

	// The cache serves the second request: no source read, no conversion.
	rr = getPreview(t, gen, "alice", "file=/report.docx&x=100&y=100")
	if rr.Code != http.StatusOK {
		t.Fatalf("cached status = %d", rr.Code)
	}
	if fake.calls != 1 {
		t.Errorf("rasterizer calls after cache hit = %d, want 1", fake.calls)
	}
}

func TestPreviewOfficeRasterizerErrors(t *testing.T) {
	cases := []struct {
		name string
		wire func(gen *Generator)
	}{
		{"nil-rasterizer", func(gen *Generator) {}},
		{"not-handled", func(gen *Generator) { gen.Rasterizer = &fakeRasterizer{handles: false} }},
		{"convert-error", func(gen *Generator) { gen.Rasterizer = &fakeRasterizer{handles: true, err: errors.New("boom")} }},
		// The converter's output re-enters the sniff: a non-image raster is
		// a 404, never served (ADR-0053's no-fallback rule).
		{"non-image-output", func(gen *Generator) { gen.Rasterizer = &fakeRasterizer{handles: true, out: []byte("not an image")} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dav, st := newTestDAV(t)
			upload(t, dav, "/report.docx", makeDocx(t))
			gen := newTestGenerator(t, dav, st, 0)
			tc.wire(gen)
			rr := getPreview(t, gen, "alice", "file=/report.docx&x=100&y=100")
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (uniform not-previewable)", rr.Code)
			}
		})
	}
}

func TestPreviewOfficePregenerate(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/report.docx", makeDocx(t))
	gen := newTestGenerator(t, dav, st, 0)
	fake := &fakeRasterizer{handles: true, out: makePNG(t, 640, 480)}
	gen.Rasterizer = fake

	if err := gen.Pregenerate(context.Background(), "alice", "/report.docx", []int{64}); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Fatalf("rasterizer calls = %d, want 1 (once per document, not per box)", fake.calls)
	}
	rr := getPreview(t, gen, "alice", "file=/report.docx&x=64&y=64")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if fake.calls != 1 {
		t.Errorf("rasterizer calls after warm cache = %d, want 1", fake.calls)
	}
}

// Pregeneration without a rasterizer keeps the head-sniff early-exit: the
// office bytes are never fully read and nothing is cached.
func TestPreviewOfficePregenerateNoRasterizer(t *testing.T) {
	dav, st := newTestDAV(t)
	upload(t, dav, "/report.docx", makeDocx(t))
	gen := newTestGenerator(t, dav, st, 0)
	if err := gen.Pregenerate(context.Background(), "alice", "/report.docx", []int{64}); err != nil {
		t.Fatal(err)
	}
	rr := getPreview(t, gen, "alice", "file=/report.docx&x=64&y=64")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 without a rasterizer", rr.Code)
	}
}

func TestExecRasterizerHandles(t *testing.T) {
	r := &ExecRasterizer{Command: "/usr/bin/soffice"}
	for _, mime := range []string{mimePDF, mimeOOXMLWord, mimeOOXMLSheet, mimeOOXMLSlide, mimeODFText, mimeODFSheet, mimeODFSlides} {
		if !r.Handles(mime) {
			t.Errorf("Handles(%q) = false", mime)
		}
	}
	for _, mime := range []string{"", "application/zip", "image/png", "application/msword"} {
		if r.Handles(mime) {
			t.Errorf("Handles(%q) = true", mime)
		}
	}
}

func TestExecRasterizerMissingBinary(t *testing.T) {
	r := &ExecRasterizer{Command: "/nonexistent/soffice"}
	if _, err := r.Rasterize(context.Background(), makeDocx(t)); err == nil {
		t.Fatal("expected an error for a missing converter binary")
	}
	var nilR *ExecRasterizer
	if _, err := nilR.Rasterize(context.Background(), makeDocx(t)); err == nil {
		t.Fatal("expected an error for a nil rasterizer")
	}
}
