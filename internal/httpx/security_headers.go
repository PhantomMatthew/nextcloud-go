package httpx

import "net/http"

// SecurityHeadersConfig controls optional headers that upstream Nextcloud
// emits only when Apache mod_headers is available. The unconditional set
// (Feature-Policy, X-Robots-Tag) is always written.
type SecurityHeadersConfig struct {
	EmitLegacyHeaders bool
}

// DefaultSecurityHeaders returns a config equivalent to a standard Nextcloud
// deployment behind Apache with mod_headers enabled.
func DefaultSecurityHeaders() SecurityHeadersConfig {
	return SecurityHeadersConfig{EmitLegacyHeaders: true}
}

// SecurityHeaders middleware emits Nextcloud's baseline security headers.
// Header values are byte-equal to upstream OC_Response::sendSecurityHeaders.
func SecurityHeaders(cfg SecurityHeadersConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Feature-Policy", "autoplay 'self';camera 'none';fullscreen 'self';geolocation 'none';microphone 'none';payment 'none'")
			h.Set("X-Robots-Tag", "noindex, nofollow")
			if cfg.EmitLegacyHeaders {
				h.Set("X-Content-Type-Options", "nosniff")
				h.Set("X-Frame-Options", "SAMEORIGIN")
				h.Set("X-Permitted-Cross-Domain-Policies", "none")
				h.Set("X-XSS-Protection", "1; mode=block")
				h.Set("Referrer-Policy", "no-referrer")
			}
			next.ServeHTTP(w, r)
		})
	}
}
