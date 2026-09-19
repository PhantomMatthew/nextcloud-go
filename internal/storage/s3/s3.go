// Package s3 implements storage.Storage on an S3-compatible bucket.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

const multipartThreshold = 8 << 20

type objectInfo struct {
	Key     string
	Size    int64
	ModTime time.Time
}

type objectAPI interface {
	stat(ctx context.Context, key string) (objectInfo, error)
	get(ctx context.Context, key string) (io.ReadSeekCloser, error)
	put(ctx context.Context, key string, r io.Reader, size int64) error
	remove(ctx context.Context, key string) error
	list(ctx context.Context, prefix string) ([]objectInfo, error)
	copy(ctx context.Context, src, dst string) error
}

// Backend is an S3-compatible storage.Storage.
type Backend struct {
	api objectAPI
}

func newBackend(api objectAPI) *Backend {
	return &Backend{api: api}
}

// New returns a Storage talking to the configured S3 endpoint.
func New(cfg config.BackendConfig) (storage.Storage, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: endpoint and bucket are required")
	}
	host, secure, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	cli, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: client: %w", err)
	}
	return newBackend(&minioStore{cli: cli, bucket: cfg.Bucket}), nil
}

func parseEndpoint(raw string) (string, bool, error) {
	if raw == "" {
		return "", false, fmt.Errorf("s3: empty endpoint")
	}
	if !strings.Contains(raw, "://") {
		return raw, true, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("s3: invalid endpoint")
	}
	return u.Host, u.Scheme == "https", nil
}

func objectKey(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", storage.ErrInvalidPath
	}
	slash := filepath.ToSlash(p)
	if strings.HasPrefix(slash, "/") {
		return "", storage.ErrInvalidPath
	}
	for _, seg := range strings.Split(slash, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", storage.ErrInvalidPath
		}
	}
	return slash, nil
}

func (b *Backend) Stat(ctx context.Context, p string) (*storage.FileInfo, error) {
	key, err := objectKey(p)
	if err != nil {
		return nil, err
	}
	return b.lookup(ctx, key, p)
}

func (b *Backend) Open(ctx context.Context, p string) (io.ReadSeekCloser, error) {
	key, err := objectKey(p)
	if err != nil {
		return nil, err
	}
	info, err := b.lookup(ctx, key, p)
	if err != nil {
		return nil, err
	}
	if info.IsDir {
		return nil, storage.ErrIsDir
	}
	return b.api.get(ctx, key)
}

func (b *Backend) Create(ctx context.Context, p string, _ int64) (io.WriteCloser, error) {
	key, err := objectKey(p)
	if err != nil {
		return nil, err
	}
	info, err := b.lookup(ctx, key, p)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if err == nil && info.IsDir {
		return nil, storage.ErrIsDir
	}
	return &putWriter{ctx: ctx, api: b.api, key: key}, nil
}

func (b *Backend) Delete(ctx context.Context, p string) error {
	key, err := objectKey(p)
	if err != nil {
		return err
	}
	info, err := b.lookup(ctx, key, p)
	if err != nil {
		return err
	}
	if info.IsDir {
		kids, err := b.api.list(ctx, key+"/")
		if err != nil {
			return err
		}
		for _, k := range kids {
			if k.Key == key+"/" {
				continue
			}
			return storage.ErrNotEmpty
		}
		if err := b.api.remove(ctx, key+"/"); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		return nil
	}
	return b.api.remove(ctx, key)
}

func (b *Backend) List(ctx context.Context, p string) ([]*storage.FileInfo, error) {
	key, err := objectKey(p)
	if err != nil {
		return nil, err
	}
	info, err := b.lookup(ctx, key, p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir {
		return nil, storage.ErrNotDir
	}
	kids, err := b.api.list(ctx, key+"/")
	if err != nil {
		return nil, err
	}
	seenDir := map[string]struct{}{}
	out := make([]*storage.FileInfo, 0, len(kids))
	base := filepath.ToSlash(p)
	for _, k := range kids {
		rel := strings.TrimPrefix(k.Key, key+"/")
		if rel == "" {
			continue
		}
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			name := rel[:i]
			if _, ok := seenDir[name]; ok {
				continue
			}
			seenDir[name] = struct{}{}
			out = append(out, &storage.FileInfo{
				Path:    base + "/" + name,
				IsDir:   true,
				ModTime: k.ModTime.UTC(),
			})
			continue
		}
		out = append(out, &storage.FileInfo{
			Path:    base + "/" + rel,
			Size:    k.Size,
			ModTime: k.ModTime.UTC(),
		})
	}
	return out, nil
}

func (b *Backend) Rename(ctx context.Context, src, dst string) error {
	from, err := objectKey(src)
	if err != nil {
		return err
	}
	to, err := objectKey(dst)
	if err != nil {
		return err
	}
	info, err := b.lookup(ctx, from, src)
	if err != nil {
		return err
	}
	if !info.IsDir {
		if err := b.api.copy(ctx, from, to); err != nil {
			return err
		}
		return b.api.remove(ctx, from)
	}
	kids, err := b.api.list(ctx, from+"/")
	if err != nil {
		return err
	}
	if _, err := b.api.stat(ctx, from+"/"); err == nil {
		kids = append([]objectInfo{{Key: from + "/"}}, kids...)
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	seen := map[string]struct{}{}
	var copied []string
	for _, k := range kids {
		if _, ok := seen[k.Key]; ok {
			continue
		}
		seen[k.Key] = struct{}{}
		rel := strings.TrimPrefix(k.Key, from)
		dstKey := to + rel
		if err := b.api.copy(ctx, k.Key, dstKey); err != nil {
			return err
		}
		copied = append(copied, k.Key)
	}
	for _, k := range copied {
		if err := b.api.remove(ctx, k); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (b *Backend) Mkdir(ctx context.Context, p string) error {
	key, err := objectKey(p)
	if err != nil {
		return err
	}
	if _, err := b.lookup(ctx, key, p); err == nil {
		return storage.ErrExists
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return b.api.put(ctx, key+"/", bytes.NewReader(nil), 0)
}

func (b *Backend) lookup(ctx context.Context, key, p string) (*storage.FileInfo, error) {
	info, err := b.api.stat(ctx, key)
	if err == nil {
		return &storage.FileInfo{
			Path:    filepath.ToSlash(p),
			Size:    info.Size,
			ModTime: info.ModTime.UTC(),
		}, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if info, err := b.api.stat(ctx, key+"/"); err == nil {
		return &storage.FileInfo{
			Path:    filepath.ToSlash(p),
			ModTime: info.ModTime.UTC(),
			IsDir:   true,
		}, nil
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	kids, err := b.api.list(ctx, key+"/")
	if err != nil {
		return nil, err
	}
	if len(kids) == 0 {
		return nil, storage.ErrNotFound
	}
	return &storage.FileInfo{
		Path:    filepath.ToSlash(p),
		ModTime: kids[0].ModTime.UTC(),
		IsDir:   true,
	}, nil
}

type putWriter struct {
	ctx    context.Context
	api    objectAPI
	key    string
	buf    bytes.Buffer
	tmp    *os.File
	closed bool
}

func (w *putWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("s3: write on closed writer")
	}
	if w.tmp != nil {
		return w.tmp.Write(p)
	}
	if w.buf.Len()+len(p) <= multipartThreshold {
		return w.buf.Write(p)
	}
	f, err := os.CreateTemp("", "ncgo-s3-*.part")
	if err != nil {
		return 0, fmt.Errorf("s3: temp: %w", err)
	}
	if _, err := w.buf.WriteTo(f); err != nil {
		name := f.Name()
		if cerr := f.Close(); cerr != nil {
			return 0, errors.Join(err, cerr)
		}
		if rerr := os.Remove(name); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return 0, errors.Join(err, rerr)
		}
		return 0, err
	}
	w.buf.Reset()
	w.tmp = f
	return w.tmp.Write(p)
}

func (w *putWriter) Close() (err error) {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.tmp != nil {
		name := w.tmp.Name()
		defer func() {
			if rerr := os.Remove(name); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}()
		if _, seekErr := w.tmp.Seek(0, io.SeekStart); seekErr != nil {
			if cerr := w.tmp.Close(); cerr != nil {
				return errors.Join(seekErr, cerr)
			}
			return seekErr
		}
		st, statErr := w.tmp.Stat()
		if statErr != nil {
			if cerr := w.tmp.Close(); cerr != nil {
				return errors.Join(statErr, cerr)
			}
			return statErr
		}
		err = w.api.put(w.ctx, w.key, w.tmp, st.Size())
		if cerr := w.tmp.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return w.api.put(w.ctx, w.key, bytes.NewReader(w.buf.Bytes()), int64(w.buf.Len()))
}
