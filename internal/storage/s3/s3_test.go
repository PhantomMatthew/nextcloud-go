package s3

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

func TestNewRequiresEndpointBucket(t *testing.T) {
	if _, err := New(config.BackendConfig{}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := New(config.BackendConfig{Endpoint: "http://127.0.0.1:9"}); err == nil {
		t.Fatal("expected bucket error")
	}
	st, err := New(config.BackendConfig{
		Endpoint:        "http://127.0.0.1:9",
		Bucket:          "ncgo",
		AccessKeyID:     "ak",
		SecretAccessKey: "sk",
		Region:          "us-east-1",
	})
	if err != nil || st == nil {
		t.Fatalf("New = %v %v", st, err)
	}
}

func TestParseEndpoint(t *testing.T) {
	host, secure, err := parseEndpoint("https://s3.example.com")
	if err != nil || host != "s3.example.com" || !secure {
		t.Fatalf("https = %q %v %v", host, secure, err)
	}
	host, secure, err = parseEndpoint("http://127.0.0.1:9000")
	if err != nil || host != "127.0.0.1:9000" || secure {
		t.Fatalf("http = %q %v %v", host, secure, err)
	}
	host, secure, err = parseEndpoint("play.min.io")
	if err != nil || host != "play.min.io" || !secure {
		t.Fatalf("bare = %q %v %v", host, secure, err)
	}
}

func TestMapErrNoSuchKey(t *testing.T) {
	err := mapErr(minio.ErrorResponse{Code: minio.NoSuchKey, StatusCode: 404})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("map = %v", err)
	}
}

func TestS3RoundTrip(t *testing.T) {
	fs := newMemBackend()
	ctx := t.Context()
	if err := fs.Mkdir(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	w, err := fs.Create(ctx, "dir/file.txt", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "data"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := fs.Stat(ctx, "dir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != 4 || st.IsDir {
		t.Fatalf("%+v", st)
	}
	r, err := fs.Open(ctx, "dir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if string(body) != "data" {
		t.Fatalf("body %q", body)
	}
	ents, err := fs.List(ctx, "dir")
	if err != nil || len(ents) != 1 {
		t.Fatalf("list = %v %v", ents, err)
	}
	if err := fs.Rename(ctx, "dir/file.txt", "dir/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "dir/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
}

func TestS3RejectsEscape(t *testing.T) {
	fs := newMemBackend()
	ctx := t.Context()
	for _, p := range []string{"../outside", "/etc/passwd", "..", ""} {
		if _, err := fs.Stat(ctx, p); !errors.Is(err, storage.ErrInvalidPath) {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestS3Errors(t *testing.T) {
	fs := newMemBackend()
	ctx := t.Context()
	if _, err := fs.Stat(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stat missing = %v", err)
	}
	if err := fs.Mkdir(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir(ctx, "d"); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("mkdir exists = %v", err)
	}
	w, err := fs.Create(ctx, "d/a.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "d"); !errors.Is(err, storage.ErrNotEmpty) {
		t.Fatalf("not empty = %v", err)
	}
	if _, err := fs.Open(ctx, "d"); !errors.Is(err, storage.ErrIsDir) {
		t.Fatalf("open dir = %v", err)
	}
	if _, err := fs.List(ctx, "d/a.txt"); !errors.Is(err, storage.ErrNotDir) {
		t.Fatalf("list file = %v", err)
	}
}

func TestS3CreateMultipartThreshold(t *testing.T) {
	fs := newMemBackend()
	ctx := t.Context()
	w, err := fs.Create(ctx, "big.bin", int64(multipartThreshold+1))
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), multipartThreshold+1)
	if _, err := w.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := fs.Stat(ctx, "big.bin")
	if err != nil || st.Size != int64(len(chunk)) {
		t.Fatalf("stat = %+v %v", st, err)
	}
}

func TestS3RenameDir(t *testing.T) {
	fs := newMemBackend()
	ctx := t.Context()
	if err := fs.Mkdir(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	w, err := fs.Create(ctx, "old/a.txt", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "z"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename(ctx, "old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(ctx, "old"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("old still present: %v", err)
	}
	if _, err := fs.Stat(ctx, "new/a.txt"); err != nil {
		t.Fatal(err)
	}
}
