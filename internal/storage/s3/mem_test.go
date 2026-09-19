package s3

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

type memObj struct {
	data []byte
	mod  time.Time
}

type memStore struct {
	mu  sync.Mutex
	obj map[string]memObj
	now func() time.Time
}

func newMemBackend() *Backend {
	return newBackend(&memStore{
		obj: map[string]memObj{},
		now: func() time.Time { return time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC) },
	})
}

func (m *memStore) stat(_ context.Context, key string) (objectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.obj[key]
	if !ok {
		return objectInfo{}, storage.ErrNotFound
	}
	return objectInfo{Key: key, Size: int64(len(o.data)), ModTime: o.mod}, nil
}

func (m *memStore) get(_ context.Context, key string) (io.ReadSeekCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.obj[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &memReader{Reader: bytes.NewReader(o.data)}, nil
}

func (m *memStore) put(_ context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(data)) != size {
		return io.ErrUnexpectedEOF
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.obj[key] = memObj{data: data, mod: m.now()}
	return nil
}

func (m *memStore) remove(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.obj[key]; !ok {
		return storage.ErrNotFound
	}
	delete(m.obj, key)
	return nil
}

func (m *memStore) list(_ context.Context, prefix string) ([]objectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []objectInfo
	for k, o := range m.obj {
		if strings.HasPrefix(k, prefix) {
			out = append(out, objectInfo{Key: k, Size: int64(len(o.data)), ModTime: o.mod})
		}
	}
	return out, nil
}

func (m *memStore) copy(_ context.Context, src, dst string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.obj[src]
	if !ok {
		return storage.ErrNotFound
	}
	cp := make([]byte, len(o.data))
	copy(cp, o.data)
	m.obj[dst] = memObj{data: cp, mod: m.now()}
	return nil
}

type memReader struct {
	*bytes.Reader
}

func (m *memReader) Close() error { return nil }
