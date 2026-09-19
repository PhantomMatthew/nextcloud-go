//go:build integration

package s3

import (
	"io"
	"os"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
)

func TestIntegrationS3RoundTrip(t *testing.T) {
	ep := os.Getenv("NCGO_S3_ENDPOINT")
	bucket := os.Getenv("NCGO_S3_BUCKET")
	if ep == "" || bucket == "" {
		t.Skip("NCGO_S3_ENDPOINT and NCGO_S3_BUCKET not set")
	}
	st, err := New(config.BackendConfig{
		Type:            "s3",
		Endpoint:        ep,
		Bucket:          bucket,
		AccessKeyID:     os.Getenv("NCGO_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("NCGO_S3_SECRET_ACCESS_KEY"),
		Region:          os.Getenv("NCGO_S3_REGION"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	w, err := st.Create(ctx, "itest/hello.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := st.Open(ctx, "itest/hello.txt")
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
	if string(body) != "hello" {
		t.Fatalf("body %q", body)
	}
	if err := st.Delete(ctx, "itest/hello.txt"); err != nil {
		t.Fatal(err)
	}
}
