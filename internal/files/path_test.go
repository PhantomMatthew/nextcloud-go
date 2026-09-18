package files

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "/", false},
		{"/", "/", false},
		{"/foo", "/foo", false},
		{"/foo/", "/foo", false},
		{"foo/bar", "/foo/bar", false},
		{"/a/../b", "", true},
		{"/a/./b", "", true},
		{"/a//b", "", true},
		{"/\x00", "", true},
	}
	for _, tt := range tests {
		got, err := NormalizePath(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("NormalizePath(%q) err=nil", tt.in)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NormalizePath(%q) = %q %v, want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestComputeFileETagStable(t *testing.T) {
	t.Parallel()
	mt := time.Unix(1_700_000_000, 0).UTC()
	a := ComputeFileETag(3, mt, 10)
	b := ComputeFileETag(3, mt, 10)
	if a == "" || a != b {
		t.Fatalf("etag %q %q", a, b)
	}
	if ComputeFileETag(4, mt, 10) == a {
		t.Fatal("different id should change etag")
	}
}

func TestComputeDirETagOrder(t *testing.T) {
	t.Parallel()
	a := []File{{Path: "/b", ETag: "bb"}, {Path: "/a", ETag: "aa"}}
	b := []File{{Path: "/a", ETag: "aa"}, {Path: "/b", ETag: "bb"}}
	if ComputeDirETag(a) != ComputeDirETag(b) {
		t.Fatal("dir etag must be path-order invariant")
	}
}

func TestValidTransferID(t *testing.T) {
	t.Parallel()
	if !ValidTransferID("3847562910") || !ValidTransferID("a_b-1") {
		t.Fatal("expected valid")
	}
	if ValidTransferID("") || ValidTransferID("../x") || ValidTransferID(strings.Repeat("a", 65)) {
		t.Fatal("expected invalid")
	}
}
