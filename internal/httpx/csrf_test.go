package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCSRF(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	tests := []struct {
		name    string
		cfg     CSRFConfig
		method  string
		path    string
		headers map[string]string
		want    int
	}{
		{
			name:   "safe method passes",
			method: http.MethodGet,
			path:   "/anything",
			want:   http.StatusNoContent,
		},
		{
			name:    "ocs api request bypasses",
			method:  http.MethodPost,
			path:    "/ocs/v2.php/cloud/user",
			headers: map[string]string{HeaderOCSAPIRequest: "true"},
			want:    http.StatusNoContent,
		},
		{
			name:    "bearer auth bypasses",
			method:  http.MethodPost,
			path:    "/foo",
			headers: map[string]string{HeaderAuthorization: "Bearer abc"},
			want:    http.StatusNoContent,
		},
		{
			name:   "unsafe without validator rejected",
			method: http.MethodPost,
			path:   "/foo",
			want:   http.StatusPreconditionFailed,
		},
		{
			name:   "path bypass exact match",
			cfg:    CSRFConfig{PathBypass: []string{"/index.php/login/v2"}},
			method: http.MethodPost,
			path:   "/index.php/login/v2",
			want:   http.StatusNoContent,
		},
		{
			name:   "path bypass non-match rejected",
			cfg:    CSRFConfig{PathBypass: []string{"/index.php/login/v2"}},
			method: http.MethodPost,
			path:   "/index.php/login/v2/extra",
			want:   http.StatusPreconditionFailed,
		},
		{
			name:   "validator pass",
			cfg:    CSRFConfig{Validate: func(*http.Request) bool { return true }},
			method: http.MethodPost,
			path:   "/foo",
			want:   http.StatusNoContent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := CSRF(tt.cfg)(ok)
			req := httptest.NewRequest(tt.method, tt.path, nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status=%d want=%d", rec.Code, tt.want)
			}
		})
	}
}
