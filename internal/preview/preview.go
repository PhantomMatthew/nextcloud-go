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
	_ "golang.org/x/image/webp" // register WebP decoding (VP8/VP8L, ADR-0087)
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
	mimeGIF            = "image/gif"
	mimeWebp           = "image/webp"
	cacheControlValue  = "private, max-age=86400"
	// modeFill is the only honoured value of the mode query parameter
	// (ADR-0086): aspect-fill with a centre crop. Anything else falls back
	// to the aspect-preserving fit.
	modeFill = "fill"
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
// mode=fill selects aspect-fill with a centre crop (ADR-0086); any other
// value, or none, keeps the aspect-preserving fit.
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
	g.serve(w, r, p.UID, np, x, y, q.Get("mode") == modeFill)
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

func (g *Generator) serve(w http.ResponseWriter, r *http.Request, uid, np string, x, y int, fill bool) {
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

	key := cacheKey(uid, np, entry.ETag, x, y, fill)
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
		return g.generate(ctx, rc, key, x, y, fill)
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

func (g *Generator) generate(ctx context.Context, rc io.Reader, key string, x, y int, fill bool) (*generated, error) {
	data, err := io.ReadAll(io.LimitReader(rc, maxSourceBytes+1))
	if err != nil || len(data) > maxSourceBytes {
		return nil, errNotPreviewable
	}
	out, err := render(data, x, y, fill)
	if err != nil {
		return nil, err
	}
	if err := g.store(ctx, key, out); err != nil {
		// The preview is still served; only reuse is lost.
		g.logger().WarnContext(ctx, "preview cache write failed", slog.Any("error", err))
	}
	return out, nil
}

// render sniffs, bounds-checks, decodes, scales, and encodes data into one
// generated preview for the x-by-y box: an aspect-preserving fit, or an
// aspect-fill with centre crop when fill is set (ADR-0086). Non-images,
// corrupt content, and over-limit source dimensions all collapse to
// errNotPreviewable.
func render(data []byte, x, y int, fill bool) (*generated, error) {
	switch http.DetectContentType(data[:min(sniffBytes, len(data))]) {
	case mimeJPEG, mimePNG, mimeGIF, mimeWebp:
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
	var dst image.Image
	if fill {
		dst = scaleFill(src, x, y)
	} else {
		dst = scale(src, x, y)
	}

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
	return out, nil
}

// Pregenerate renders the configured hot-size boxes for uid/path into the
// cache ahead of any client request (ADR-0084). It runs from the jobs
// runner, which retries a failed Run forever, so the error contract differs
// from serve: only infrastructure failures (the source Read itself) are
// returned for retry. Every per-file condition — missing, unreadable,
// non-image, oversized, corrupt — is a quiet nil, mirroring the endpoint's
// uniform-404 philosophy.
func (g *Generator) Pregenerate(ctx context.Context, uid, path string, boxes []int) error {
	if g == nil || g.Source == nil || g.Cache == nil {
		return nil
	}
	np, err := files.NormalizePath(path)
	if err != nil || np == "/" {
		return nil
	}
	rc, entry, err := g.Source.Read(ctx, uid, np)
	if err != nil {
		switch {
		case errors.Is(err, webdav.ErrNotFound),
			errors.Is(err, webdav.ErrForbidden),
			errors.Is(err, webdav.ErrIsDir):
			return nil
		default:
			return err
		}
	}
	defer rc.Close()

	// Head-sniff before the full read: unlike serve (which a client asked
	// for), this background path must not ReadAll up to 256MiB of every
	// uploaded video.
	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	head = head[:n]
	if n == 0 {
		return nil
	}
	switch http.DetectContentType(head) {
	case mimeJPEG, mimePNG, mimeGIF, mimeWebp:
	default:
		return nil
	}
	rest, err := io.ReadAll(io.LimitReader(rc, int64(maxSourceBytes+1-n)))
	if err != nil {
		return nil
	}
	data := make([]byte, 0, n+len(rest))
	data = append(data, head...)
	data = append(data, rest...)
	if len(data) > maxSourceBytes {
		return nil
	}

	for _, b := range boxes {
		key := cacheKey(uid, np, entry.ETag, b, b, false)
		if f, _, _ := g.openCached(ctx, key); f != nil {
			_ = f.Close()
			continue
		}
		_, err, _ := g.group.Do(key, func() (any, error) {
			// A concurrent run (or a serve request) may have filled the
			// cache while this one waited on the flight.
			if f, _, _ := g.openCached(ctx, key); f != nil {
				_ = f.Close()
				return nil, nil
			}
			// Pregeneration warms fit-mode boxes only (ADR-0086).
			out, rerr := render(data, b, b, false)
			if rerr != nil {
				return nil, rerr
			}
			if serr := g.store(ctx, key, out); serr != nil {
				// Reuse is lost, but the next run rebuilds; never fatal.
				g.logger().WarnContext(ctx, "preview cache write failed", slog.Any("error", serr))
			}
			return out, nil
		})
		if err != nil {
			// Not-previewable fails every box identically; anything else is
			// infrastructure and worth a runner retry.
			if errors.Is(err, errNotPreviewable) {
				return nil
			}
			return err
		}
	}
	return nil
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

// scaleFill returns src covering the x-by-y box, centre-cropped to it
// (ADR-0086). Like scale it never upscales: a source smaller than the box
// keeps its size, so the output is min(box, scaled) per edge.
func scaleFill(src image.Image, x, y int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	factor := min(max(float64(x)/float64(w), float64(y)/float64(h)), 1.0)
	dw := max(1, int(math.Round(float64(w)*factor)))
	dh := max(1, int(math.Round(float64(h)*factor)))
	scaled := src
	if dw != w || dh != h {
		s := image.NewRGBA(image.Rect(0, 0, dw, dh))
		draw.ApproxBiLinear.Scale(s, s.Bounds(), src, b, draw.Over, nil)
		scaled = s
	}
	cw, ch := min(dw, x), min(dh, y)
	if cw == dw && ch == dh {
		return scaled
	}
	// Copy rather than SubImage so non-zero source bounds stay correct.
	sb := scaled.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, cw, ch))
	draw.Draw(dst, dst.Bounds(), scaled, image.Pt(sb.Min.X+(dw-cw)/2, sb.Min.Y+(dh-ch)/2), draw.Src)
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
// change (new etag) invalidates implicitly. Fill mode appends a marker
// (ADR-0086); the fit string is byte-identical to the pre-fill format, so
// existing fit cache entries stay valid.
func cacheKey(uid, path, etag string, x, y int, fill bool) string {
	s := uid + "\n" + path + "\n" + etag + "\n" + strconv.Itoa(x) + "x" + strconv.Itoa(y)
	if fill {
		s += "\nfill"
	}
	sum := sha256.Sum256([]byte(s))
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
