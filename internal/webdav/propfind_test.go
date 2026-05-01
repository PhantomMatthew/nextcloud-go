package webdav

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteMultistatus_EmptyHomeRoot(t *testing.T) {
	root := &Entry{
		Path:        "/",
		IsDir:       true,
		ETag:        "00000000000000000000000000000000",
		ModTime:     time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC),
		NumericID:   1,
		Permissions: PermAll,
		Shareable:   true,
		ContentType: "httpd/unix-directory",
	}
	var buf bytes.Buffer
	WriteMultistatus(&buf, PropfindContext{
		BaseHref:   "/remote.php/dav/files/alice/",
		InstanceID: "oc123abc",
	}, []*Entry{root})

	out := buf.String()
	mustContain := []string{
		`<?xml version="1.0"?>`,
		`<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`,
		`<d:response>`,
		`<d:href>/remote.php/dav/files/alice/</d:href>`,
		`<d:resourcetype><d:collection/></d:resourcetype>`,
		`<d:getetag>&quot;00000000000000000000000000000000&quot;</d:getetag>`,
		`<d:getlastmodified>Thu, 01 May 2025 12:00:00 GMT</d:getlastmodified>`,
		`<oc:id>00000001oc123abc</oc:id>`,
		`<oc:fileid>00000001oc123abc</oc:fileid>`,
		`<oc:permissions>RGDNVCK</oc:permissions>`,
		`<oc:size>0</oc:size>`,
		`<d:status>HTTP/1.1 200 OK</d:status>`,
		`</d:multistatus>`,
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\n--- got ---\n%s", s, out)
		}
	}
	if strings.Contains(out, `<d:getcontentlength>`) {
		t.Errorf("collection should NOT emit getcontentlength")
	}
	if strings.Contains(out, `<d:getcontenttype>`) {
		t.Errorf("collection should NOT emit getcontenttype")
	}
}

func TestWriteMultistatus_File(t *testing.T) {
	f := &Entry{
		Path:        "/hello.txt",
		IsDir:       false,
		Size:        13,
		ETag:        "abc123",
		ModTime:     time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC),
		NumericID:   42,
		Permissions: PermRead | PermUpdate | PermDelete,
		ContentType: "text/plain",
	}
	var buf bytes.Buffer
	WriteMultistatus(&buf, PropfindContext{
		BaseHref:   "/remote.php/dav/files/alice/",
		InstanceID: "oc123abc",
	}, []*Entry{f})

	out := buf.String()
	mustContain := []string{
		`<d:href>/remote.php/dav/files/alice/hello.txt</d:href>`,
		`<d:resourcetype/>`,
		`<d:getcontentlength>13</d:getcontentlength>`,
		`<d:getcontenttype>text/plain</d:getcontenttype>`,
		`<oc:id>00000042oc123abc</oc:id>`,
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\n--- got ---\n%s", s, out)
		}
	}
	if strings.Contains(out, `<oc:size>`) {
		t.Errorf("file should NOT emit oc:size")
	}
}

func TestBuildHref_EncodesPath(t *testing.T) {
	cases := []struct {
		base string
		e    *Entry
		want string
	}{
		{"/remote.php/dav/files/alice/", &Entry{Path: "/", IsDir: true}, "/remote.php/dav/files/alice/"},
		{"/remote.php/dav/files/alice/", &Entry{Path: "/hello.txt", IsDir: false}, "/remote.php/dav/files/alice/hello.txt"},
		{"/remote.php/dav/files/alice/", &Entry{Path: "/sub", IsDir: true}, "/remote.php/dav/files/alice/sub/"},
		{"/remote.php/dav/files/alice/", &Entry{Path: "/a b.txt", IsDir: false}, "/remote.php/dav/files/alice/a%20b.txt"},
	}
	for _, c := range cases {
		got := buildHref(c.base, c.e)
		if got != c.want {
			t.Errorf("buildHref(%q, %+v) = %q, want %q", c.base, c.e, got, c.want)
		}
	}
}

func TestGolden_PropfindHomeDepth0(t *testing.T) {
	root := emptyHomeRootEntry()
	var buf bytes.Buffer
	WriteMultistatus(&buf, PropfindContext{
		BaseHref:   "/remote.php/dav/files/alice/",
		InstanceID: "oc123abc",
	}, []*Entry{root})
	assertGolden(t, "home_depth0.xml", buf.Bytes())
}

func TestGolden_PropfindHomeDepth1(t *testing.T) {
	root := emptyHomeRootEntry()
	var buf bytes.Buffer
	WriteMultistatus(&buf, PropfindContext{
		BaseHref:   "/remote.php/dav/files/alice/",
		InstanceID: "oc123abc",
	}, []*Entry{root})
	assertGolden(t, "home_depth1.xml", buf.Bytes())
}

func emptyHomeRootEntry() *Entry {
	return &Entry{
		Path:        "/",
		IsDir:       true,
		ETag:        "00000000000000000000000000000000",
		ModTime:     time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC),
		NumericID:   1,
		Permissions: PermAll,
		Shareable:   true,
		ContentType: "httpd/unix-directory",
	}
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", "propfind", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden %s mismatch\n--- want (%d bytes) ---\n%s\n--- got (%d bytes) ---\n%s", name, len(want), want, len(got), got)
	}
}
