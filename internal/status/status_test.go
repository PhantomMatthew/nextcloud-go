package status

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandlerByteExactGolden(t *testing.T) {
	p := Provider{}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status.php", nil)
	p.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin: got %q want *", got)
	}

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "status", "normal.json")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got := rr.Body.Bytes(); string(got) != string(want) {
		t.Errorf("body mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestHandlerFlags(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		want string
	}{
		{
			name: "all_false",
			p:    Provider{},
			want: `{"installed":false,"maintenance":false,"needsDbUpgrade":false,"version":"26.0.0.6","versionstring":"26.0.0 beta 4","edition":"","productname":"Nextcloud","extendedSupport":false}`,
		},
		{
			name: "installed_true",
			p:    Provider{Installed: true},
			want: `{"installed":true,"maintenance":false,"needsDbUpgrade":false,"version":"26.0.0.6","versionstring":"26.0.0 beta 4","edition":"","productname":"Nextcloud","extendedSupport":false}`,
		},
		{
			name: "all_true",
			p:    Provider{Installed: true, Maintenance: true, NeedsDBUpgrade: true, ExtendedSupport: true},
			want: `{"installed":true,"maintenance":true,"needsDbUpgrade":true,"version":"26.0.0.6","versionstring":"26.0.0 beta 4","edition":"","productname":"Nextcloud","extendedSupport":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/status.php", nil)
			tc.p.Handler().ServeHTTP(rr, req)
			if got := rr.Body.String(); got != tc.want {
				t.Errorf("body:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestHandlerAcceptsAnyMethod(t *testing.T) {
	methods := []string{http.MethodGet, http.MethodPost, http.MethodHead, http.MethodPut, http.MethodDelete}
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(m, "/status.php", nil)
			Provider{}.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("method %s: got %d want 200", m, rr.Code)
			}
		})
	}
}
