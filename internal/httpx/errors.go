package httpx

import (
	"net/http"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

// WriteOCSError emits an OCS envelope describing an error condition. It
// negotiates the response format from the request, sets the appropriate
// HTTP status (per ocs.Map for the version), and writes the body.
func WriteOCSError(w http.ResponseWriter, r *http.Request, ocsCode int, message string) {
	version := ocs.V1
	if strings.HasPrefix(r.URL.Path, "/ocs/v2.php") {
		version = ocs.V2
	}
	format := ocs.NegotiateFormat(r.URL.Query().Get("format"), r.Header.Get("Accept"))
	body, contentType, err := ocs.Render(version, format, ocs.Meta{
		Status:     "failure",
		StatusCode: ocsCode,
		Message:    message,
	}, nil)
	httpStatus := ocs.Map(version, ocsCode)
	if err != nil {
		w.WriteHeader(httpStatus)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(httpStatus)
	_, _ = w.Write(body)
}

// WritePlainError writes a minimal text/plain error response. It is the
// fallback for non-OCS endpoints that do not have a richer error format.
func WritePlainError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message))
	_, _ = w.Write([]byte("\n"))
}
