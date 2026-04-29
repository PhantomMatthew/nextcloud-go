package goldentest_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/goldentest"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestReferenceCaseParses(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "testdata", "golden", "status", "001-status-php-anonymous")

	c, err := goldentest.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RequestRaw == nil || c.ResponseRaw == nil {
		t.Fatal("Load: missing wire bytes")
	}

	if _, err := goldentest.ParseRequest(c.RequestRaw); err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	resp, err := goldentest.ParseResponse(c.ResponseRaw)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status: want 200, got %d", resp.Status)
	}
	if got := resp.Headers.Get("Content-Type"); got == "" {
		t.Fatal("Content-Type header missing")
	}
}
