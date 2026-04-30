package httpx

import (
	"context"
	"crypto/rand"
	"net/http"
)

const (
	HeaderRequestID = "X-Request-Id"

	requestIDLength   = 20
	requestIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

type ctxKey int

const ctxKeyRequestID ctxKey = iota

// RequestIDFromContext returns the request ID stored on ctx, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// WithRequestID returns ctx annotated with id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// generateRequestID returns a 20-character CSPRNG identifier matching upstream
// Nextcloud's IRequest::getId() format ([A-Za-z0-9]{20}).
func generateRequestID() string {
	buf := make([]byte, requestIDLength)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = requestIDAlphabet[0]
		}
		return string(buf)
	}
	for i, b := range buf {
		buf[i] = requestIDAlphabet[int(b)%len(requestIDAlphabet)]
	}
	return string(buf)
}

// RequestID middleware assigns a server-generated request ID to every request,
// surfaces it on the response as X-Request-Id, and stores it in the request
// context. Upstream Nextcloud always generates server-side and never honors an
// inbound X-Request-Id header.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if id == "" {
				id = generateRequestID()
			}
			w.Header().Set(HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
		})
	}
}
