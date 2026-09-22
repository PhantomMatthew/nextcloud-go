// Package preview generates and caches scaled-down previews of image files.
package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register GIF decoding (first frame)
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"golang.org/x/image/draw"
	"golang.org/x/sync/singleflight"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const (
	// defaultDimension is the box edge used when x/y are absent.
	defaultDimension = 32
	// defaultMaxDim applies when the generator is built without a cap.
	defaultMaxDim = 2048
	// maxSourceBytes caps how much of an original is read into memory.
	maxSourceBytes = 256 << 20
	// maxSourceDimension rejects DecodeConfig headers beyond this edge
	// before any pixel data is decoded (decompression-bomb guard).
	maxSourceDimension = 8192
	sniffBytes         = 512
	jpegQuality        = 85
	extJPEG            = ".jpg"
	extPNG             = ".png"
	mimeJPEG           = "image/jpeg"
	mimePNG            = "image/png"
	cacheControlValue  = "private, max-age=86400"
)

// errNotPreviewable marks originals that cannot yield a preview (non-image,
// encrypted/opaque blobs, corrupt or oversized content). It always maps to
// 404 so failed parses never serve source bytes and never distinguish
// "missing" from "unreadable".
var errNotPreviewable = errors.New("preview: not a previewable image")

// Source is the file-access gate originals are read through. *files.DAV
// satisfies it; preview access is defined as read access.
type Source interface {
	Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error)
}

// Generator serves scaled image previews with an on-disk cache.
type Generator struct {
	Source      Source
	Cache       storage.Storage
	CachePrefix string
	MaxDim      int
	Logger      *slog.Logger

	group singleflight.Group
}

// NewGenerator returns a Generator. maxDim <= 0 falls back to 2048.
func NewGenerator(src Source, cache storage.Storage, cachePrefix string, maxDim int, logger *slog.Logger) *Generator {
	if maxDim <= 0 {
		maxDim = defaultMaxDim
	}
	return &Generator{
		Source:      src,
		Cache:       cache,
		CachePrefix: cachePrefix,
		MaxDim:      maxDim,
		Logger:      logger,
	}
}

func (g *Generator) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

// ServeHTTP handles GET /index.php/core/preview[.png]. Query parameters:
// file (required DAV path), x/y (box edges, default 32, clamped to MaxDim).
// mode is ignored in v1: scaling is always an aspect-preserving fit.
func (g *Generator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.UserFromContext(r.Context())
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Authorisation Required"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	raw := q.Get("file")
	if raw == "" {
		http.Error(w, "missing file parameter", http.StatusBadRequest)
		return
	}
	np, err := files.NormalizePath(raw)
	if err != nil || np == "/" {
		http.Error(w, "invalid file path", http.StatusBadRequest)
		return
	}
	x, err := parseDimension(q.Get("x"), g.MaxDim)
	if err != nil {
		http.Error(w, "invalid x parameter", http.StatusBadRequest)
		return
	}
	y, err := parseDimension(q.Get("y"), g.MaxDim)
	if err != nil {
		http.Error(w, "invalid y parameter", http.StatusBadRequest)
		return
	}
	g.serve(w, r, p.UID, np, x, y)
}

// parseDimension accepts a positive box edge and clamps it to maxDim.
func parseDimension(raw string, maxDim int) (int, error) {
	if raw == "" {
		return defaultDimension, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("preview: bad dimension %q", raw)
	}
	if n > maxDim {
		n = maxDim
	}
	return n, nil
}

func (g *Generator) serve(w http.ResponseWriter, r *http.Request, uid, np string, x, y int) {
	ctx := r.Context()
	rc, entry, err := g.Source.Read(ctx, uid, np)
	if err != nil {
		switch {
		case errors.Is(err, webdav.ErrNotFound),
			errors.Is(err, webdav.ErrForbidden),
			errors.Is(err, webdav.ErrIsDir):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			g.logger().ErrorContext(ctx, "preview source read failed", slog.String("path", np), slog.Any("error", err))
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	defer rc.Close()

	key := cacheKey(uid, np, entry.ETag, x, y)
	if f, info, ct := g.openCached(ctx, key); f != nil {
		defer f.Close()
		writeHeaders(w, ct, key, info.Size)
		_, _ = io.Copy(w, f)
		return
	}

	res, err, _ := g.group.Do(key, func() (any, error) {
		// A concurrent request may have filled the cache while this one
		// waited on the flight.
		if f, _, ct := g.openCached(ctx, key); f != nil {
			data, rerr := io.ReadAll(f)
			_ = f.Close()
			if rerr == nil {
				return &generated{data: data, contentType: ct}, nil
			}
		}
		return g.generate(ctx, rc, key, x, y)
	})
	if err != nil {
		if errors.Is(err, errNotPreviewable) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		g.logger().ErrorContext(ctx, "preview generation failed", slog.String("path", np), slog.Any("error", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out, ok := res.(*generated)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeHeaders(w, out.contentType, key, int64(len(out.data)))
	_, _ = w.Write(out.data)
}

// generated is one rendered preview, shared with singleflight followers so
// they serve the identical bytes that were written to the cache.
type generated struct {
	data        []byte
	contentType string
}

func (g *Generator) generate(ctx context.Context, rc io.Reader, key string, x, y int) (*generated, error) {
	data, err := io.ReadAll(io.LimitReader(rc, maxSourceBytes+1))
	if err != nil || len(data) > maxSourceBytes {
		return nil, errNotPreviewable
	}
	switch http.DetectContentType(data[:min(sniffBytes, len(data))]) {
	case mimeJPEG, mimePNG, "image/gif":
	default:
		return nil, errNotPreviewable
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errNotPreviewable
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxSourceDimension || cfg.Height > maxSourceDimension {
		return nil, errNotPreviewable
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errNotPreviewable
	}
	dst := scale(src, x, y)

	out := &generated{}
	var buf bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, fmt.Errorf("preview: encode jpeg: %w", err)
		}
		out.contentType = mimeJPEG
	} else {
		if err := png.Encode(&buf, dst); err != nil {
			return nil, fmt.Errorf("preview: encode png: %w", err)
		}
		out.contentType = mimePNG
	}
	out.data = buf.Bytes()

	if err := g.store(ctx, key, out); err != nil {
		// The preview is still served; only reuse is lost.
		g.logger().WarnContext(ctx, "preview cache write failed", slog.Any("error", err))
	}
	return out, nil
}

// scale returns src fit into the x-by-y box, never upscaling. Sources that
// already fit are returned unscaled (still re-encoded by the caller, which
// strips metadata such as EXIF).
func scale(src image.Image, x, y int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	factor := min(float64(x)/float64(w), float64(y)/float64(h), 1.0)
	dw := max(1, int(math.Round(float64(w)*factor)))
	dh := max(1, int(math.Round(float64(h)*factor)))
	if dw == w && dh == h {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

func (g *Generator) store(ctx context.Context, key string, out *generated) error {
	wc, err := g.Cache.Create(ctx, g.cachedPath(key, extFor(out.contentType)), int64(len(out.data)))
	if err != nil {
		return err
	}
	_, werr := wc.Write(out.data)
	cerr := wc.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// openCached returns an open cached preview for key, trying both output
// extensions; the etag-keyed cache name makes either one authoritative.
func (g *Generator) openCached(ctx context.Context, key string) (io.ReadSeekCloser, *storage.FileInfo, string) {
	for _, ext := range []string{extJPEG, extPNG} {
		p := g.cachedPath(key, ext)
		info, err := g.Cache.Stat(ctx, p)
		if err != nil {
			continue
		}
		f, err := g.Cache.Open(ctx, p)
		if err != nil {
			continue
		}
		return f, info, mimeForExt(ext)
	}
	return nil, nil, ""
}

func (g *Generator) cachedPath(key, ext string) string {
	return g.CachePrefix + "/" + key + ext
}

// cacheKey fingerprints user, path, source etag, and box so any content
// change (new etag) invalidates implicitly.
func cacheKey(uid, path, etag string, x, y int) string {
	sum := sha256.Sum256([]byte(uid + "\n" + path + "\n" + etag + "\n" + strconv.Itoa(x) + "x" + strconv.Itoa(y)))
	return hex.EncodeToString(sum[:])
}

func extFor(contentType string) string {
	if contentType == mimeJPEG {
		return extJPEG
	}
	return extPNG
}

func mimeForExt(ext string) string {
	if ext == extJPEG {
		return mimeJPEG
	}
	return mimePNG
}

func writeHeaders(w http.ResponseWriter, contentType, etag string, size int64) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", cacheControlValue)
	h.Set("ETag", `"`+etag+`"`)
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
}
