package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/minio/minio-go/v7"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

type minioStore struct {
	cli    *minio.Client
	bucket string
}

func (m *minioStore) stat(ctx context.Context, key string) (objectInfo, error) {
	info, err := m.cli.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return objectInfo{}, mapErr(err)
	}
	return objectInfo{Key: key, Size: info.Size, ModTime: info.LastModified}, nil
}

func (m *minioStore) get(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	obj, err := m.cli.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, mapErr(err)
	}
	if _, err := obj.Stat(); err != nil {
		if cerr := obj.Close(); cerr != nil {
			return nil, errors.Join(mapErr(err), cerr)
		}
		return nil, mapErr(err)
	}
	return obj, nil
}

func (m *minioStore) put(ctx context.Context, key string, r io.Reader, size int64) error {
	opts := minio.PutObjectOptions{}
	if size > multipartThreshold {
		opts.PartSize = multipartThreshold
	}
	_, err := m.cli.PutObject(ctx, m.bucket, key, r, size, opts)
	if err != nil {
		return fmt.Errorf("s3: put: %w", mapErr(err))
	}
	return nil
}

func (m *minioStore) remove(ctx context.Context, key string) error {
	if err := m.cli.RemoveObject(ctx, m.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return mapErr(err)
	}
	return nil
}

func (m *minioStore) list(ctx context.Context, prefix string) ([]objectInfo, error) {
	var out []objectInfo
	for obj := range m.cli.ListObjects(ctx, m.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, mapErr(obj.Err)
		}
		out = append(out, objectInfo{Key: obj.Key, Size: obj.Size, ModTime: obj.LastModified})
	}
	return out, nil
}

func (m *minioStore) copy(ctx context.Context, src, dst string) error {
	_, err := m.cli.CopyObject(ctx, minio.CopyDestOptions{Bucket: m.bucket, Object: dst}, minio.CopySrcOptions{Bucket: m.bucket, Object: src})
	if err != nil {
		return mapErr(err)
	}
	return nil
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case minio.NoSuchKey, "NotFound", "NoSuchObject":
		return storage.ErrNotFound
	default:
		if resp.StatusCode == http.StatusNotFound {
			return storage.ErrNotFound
		}
		return err
	}
}
