package preview

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// mimePDF and mimeZIP are the two sniff results that can carry an office
// document: PDF exactly, and every OOXML/ODF container as application/zip.
// ADR-0053's sniff-first stance holds: the uploader-supplied MIME is never
// trusted — a ZIP is dispatched only after container inspection.
const (
	mimePDF = "application/pdf"
	mimeZIP = "application/zip"
)

// Canonical office mimetypes officeMime refines a document into. A
// Rasterizer's Handles is consulted with these values only — never a raw
// sniff result or an uploader claim.
const (
	mimeOOXMLWord  = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeOOXMLSheet = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimeOOXMLSlide = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mimeODFText    = "application/vnd.oasis.opendocument.text"
	mimeODFSheet   = "application/vnd.oasis.opendocument.spreadsheet"
	mimeODFSlides  = "application/vnd.oasis.opendocument.presentation"
)

// Rasterizer converts non-image source bytes (office documents) into raster
// image bytes the builtin pipeline then bounds-checks, scales, and encodes
// like any decoded original. Wired when the admin configures an external
// renderer (previews.office_*); nil keeps the image-sniff whitelist
// bit-identical to pre-office behavior.
type Rasterizer interface {
	// Handles reports whether mime (a canonical type from officeMime) is
	// convertible.
	Handles(mime string) bool
	// Rasterize renders the first page/sheet of data into JPEG/PNG/GIF/WebP
	// bytes. Any error collapses to the uniform not-previewable 404 — the
	// source bytes are never served (ADR-0053's no-fallback rule).
	Rasterize(ctx context.Context, data []byte) ([]byte, error)
}

// sourceImage returns raster bytes for the render pipeline: data itself
// when it sniffs as a supported image, else the configured Rasterizer's
// conversion (whose output render re-sniffs like any original).
func (g *Generator) sourceImage(ctx context.Context, data []byte) ([]byte, error) {
	if sniffIsImage(data) {
		return data, nil
	}
	return g.rasterize(ctx, data)
}

// rasterize runs the office fallback for bytes the image sniff rejected:
// container discrimination decides whether the Rasterizer is consulted at
// all — a plain archive or an unhandled type stays a 404 without ever
// invoking the external converter.
func (g *Generator) rasterize(ctx context.Context, data []byte) ([]byte, error) {
	if g.Rasterizer == nil {
		return nil, errNotPreviewable
	}
	mime, ok := officeMime(data)
	if !ok || !g.Rasterizer.Handles(mime) {
		return nil, errNotPreviewable
	}
	out, err := g.Rasterizer.Rasterize(ctx, data)
	if err != nil {
		// A conversion failure is a per-file condition: the endpoint's
		// uniform 404, never a 500 distinguishing "unreadable".
		g.logger().DebugContext(ctx, "preview office rasterize failed", slog.Any("error", err))
		return nil, errNotPreviewable
	}
	return out, nil
}

func sniffIsImage(data []byte) bool {
	switch http.DetectContentType(data[:min(sniffBytes, len(data))]) {
	case mimeJPEG, mimePNG, mimeGIF, mimeWebp:
		return true
	}
	return false
}

// officeMime refines sniffed document bytes into the canonical mimetype a
// Rasterizer is consulted with: PDF passes through; a ZIP container is
// inspected for the OOXML [Content_Types].xml manifest plus a main-part
// entry, or the ODF mimetype entry. A plain archive is not office.
func officeMime(data []byte) (string, bool) {
	switch http.DetectContentType(data[:min(sniffBytes, len(data))]) {
	case mimePDF:
		return mimePDF, true
	case mimeZIP:
	default:
		return "", false
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", false
	}
	names := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if names["[Content_Types].xml"] {
		switch {
		case names["word/document.xml"]:
			return mimeOOXMLWord, true
		case names["xl/workbook.xml"]:
			return mimeOOXMLSheet, true
		case names["ppt/presentation.xml"]:
			return mimeOOXMLSlide, true
		}
		return "", false
	}
	if names["mimetype"] {
		return odfMime(zr)
	}
	return "", false
}

// odfMime reads the ODF mimetype entry (the container's first, uncompressed
// member by spec) and returns its content when it names an ODF document.
func odfMime(zr *zip.Reader) (string, bool) {
	for _, f := range zr.File {
		if f.Name != "mimetype" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", false
		}
		buf, err := io.ReadAll(io.LimitReader(rc, 128))
		_ = rc.Close()
		if err != nil {
			return "", false
		}
		mime := strings.TrimSpace(string(buf))
		if strings.HasPrefix(mime, "application/vnd.oasis.opendocument.") {
			return mime, true
		}
		return "", false
	}
	return "", false
}
