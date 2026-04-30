package capabilities

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

var update = flag.Bool("update", false, "regenerate golden fixtures")

func newHandler() Handler {
	m := NewManager()
	m.Register(DefaultCoreProvider())
	return Handler{Manager: m}
}

func TestHandlerGolden(t *testing.T) {
	cases := []struct {
		name   string
		ver    ocs.Version
		format string
		accept string
		golden string
		ctype  string
	}{
		{"v1_json", ocs.V1, "json", "", "v1.json", "application/json; charset=utf-8"},
		{"v1_xml", ocs.V1, "", "", "v1.xml", "application/xml; charset=utf-8"},
		{"v2_json", ocs.V2, "json", "", "v2.json", "application/json; charset=utf-8"},
		{"v2_xml", ocs.V2, "", "", "v2.xml", "application/xml; charset=utf-8"},
	}
	h := newHandler()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			url := "/ocs/v2.php/cloud/capabilities"
			if tc.format != "" {
				url += "?format=" + tc.format
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			h.ServeOCS(tc.ver).ServeHTTP(rr, req)

			wantStatus := http.StatusOK
			if tc.ver == ocs.V2 {
				wantStatus = ocs.StatusOKv2
			}
			if rr.Code != wantStatus {
				t.Fatalf("status: got %d want %d", rr.Code, wantStatus)
			}
			if got := rr.Header().Get("Content-Type"); got != tc.ctype {
				t.Errorf("Content-Type: got %q want %q", got, tc.ctype)
			}
			if got := rr.Header().Get("ETag"); got == "" {
				t.Error("ETag: missing")
			}

			path := filepath.Join("testdata", "golden", "capabilities", tc.golden)
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, rr.Body.Bytes(), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if got := rr.Body.Bytes(); string(got) != string(want) {
				t.Errorf("body mismatch:\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestETagDeterministic(t *testing.T) {
	h := newHandler()
	etag := func(ver ocs.Version, format string) string {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/?format="+format, nil)
		h.ServeOCS(ver).ServeHTTP(rr, req)
		return rr.Header().Get("ETag")
	}
	a := etag(ocs.V2, "json")
	b := etag(ocs.V2, "json")
	if a != b {
		t.Errorf("ETag not stable across calls: %q vs %q", a, b)
	}
	if a == "" || a == `""` {
		t.Errorf("ETag empty: %q", a)
	}
	xml := etag(ocs.V2, "xml")
	if a != xml {
		t.Errorf("ETag depends on format: json=%q xml=%q", a, xml)
	}
	v1 := etag(ocs.V1, "json")
	if a != v1 {
		t.Errorf("ETag depends on version: v2=%q v1=%q", a, v1)
	}
}
