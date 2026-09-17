package api

import "net/http"

// Route is a module-registered HTTP endpoint.
type Route struct {
	Method       string
	Pattern      string
	Handler      http.Handler
	RequiresAuth bool
	Transport    string
}
