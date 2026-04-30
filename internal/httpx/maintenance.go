package httpx

import (
	"net/http"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

const (
	HeaderMaintenanceMode = "X-Nextcloud-Maintenance-Mode"
	HeaderRetryAfter      = "Retry-After"

	maintenanceRetryAfter = "120"
	maintenanceMessage    = "Service unavailable"
)

// MaintenanceProvider reports whether the instance is in maintenance mode.
// Implementations must be safe for concurrent use.
type MaintenanceProvider interface {
	InMaintenance() bool
}

// MaintenanceFunc adapts a function to MaintenanceProvider.
type MaintenanceFunc func() bool

func (f MaintenanceFunc) InMaintenance() bool { return f() }

// Maintenance middleware short-circuits requests with HTTP 503 when the
// instance is in maintenance mode, mirroring upstream Nextcloud:
//   - /status.php is always allowed through (returns the maintenance flag in JSON).
//   - /ocs/* responds with the OCS envelope (failure, statuscode 503).
//   - All other paths receive a minimal HTML page plus Retry-After: 120.
//
// X-Nextcloud-Maintenance-Mode: 1 is set on every 503 response.
func Maintenance(provider MaintenanceProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !provider.InMaintenance() {
				next.ServeHTTP(w, r)
				return
			}
			if r.URL.Path == "/status.php" {
				next.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/ocs/") {
				writeMaintenanceOCS(w, r)
				return
			}
			writeMaintenanceHTML(w)
		})
	}
}

func writeMaintenanceOCS(w http.ResponseWriter, r *http.Request) {
	version := ocs.V1
	if strings.HasPrefix(r.URL.Path, "/ocs/v2.php") {
		version = ocs.V2
	}
	format := ocs.NegotiateFormat(r.URL.Query().Get("format"), r.Header.Get("Accept"))
	meta := ocs.Meta{
		Status:     "failure",
		StatusCode: http.StatusServiceUnavailable,
		Message:    maintenanceMessage,
	}
	body, contentType, err := ocs.Render(version, format, meta, nil)
	if err != nil {
		w.Header().Set(HeaderMaintenanceMode, "1")
		w.Header().Set(HeaderRetryAfter, maintenanceRetryAfter)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	h.Set(HeaderMaintenanceMode, "1")
	h.Set(HeaderRetryAfter, maintenanceRetryAfter)
	h.Set("Content-Type", contentType)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}

func writeMaintenanceHTML(w http.ResponseWriter) {
	body := []byte("<!DOCTYPE html><html><head><title>Maintenance</title></head><body><h1>Service unavailable</h1><p>This server is currently in maintenance mode.</p></body></html>")
	h := w.Header()
	h.Set(HeaderMaintenanceMode, "1")
	h.Set(HeaderRetryAfter, maintenanceRetryAfter)
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}
