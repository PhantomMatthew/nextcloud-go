package httpx

import (
	"net/http"
	"sort"
	"strings"
)

// Route describes a single registered endpoint: an HTTP method, an exact
// or prefix path, the handler, and any per-route middleware that should
// wrap the handler in addition to the router's default chain.
type Route struct {
	Method      string
	Path        string
	Prefix      bool
	Handler     http.Handler
	Middlewares []Middleware
}

// Router is a stdlib-only HTTP dispatcher with two-tier path matching
// (exact then longest-prefix), explicit method routing, and per-route
// middleware composition layered on top of an optional default chain.
//
// It is intentionally minimal: no path parameters, no regex matching.
// Wildcard segments and parameter extraction belong to higher-level
// modules (e.g. WebDAV, OCS) that need them.
type Router struct {
	defaultChain []Middleware
	notFound     http.Handler
	methodNotAll http.Handler

	exact   map[string]map[string]http.Handler
	prefix  []prefixRoute
	allowed map[string]map[string]struct{}
}

type prefixRoute struct {
	method  string
	path    string
	handler http.Handler
}

// NewRouter constructs a router whose registered handlers are wrapped by
// the provided default middleware chain, in addition to any per-route
// middleware passed at registration time.
func NewRouter(defaultChain ...Middleware) *Router {
	return &Router{
		defaultChain: defaultChain,
		exact:        make(map[string]map[string]http.Handler),
		allowed:      make(map[string]map[string]struct{}),
	}
}

// SetNotFound overrides the 404 handler. If unset, http.NotFoundHandler is used.
func (r *Router) SetNotFound(h http.Handler) {
	r.notFound = h
}

// SetMethodNotAllowed overrides the 405 handler. If unset, a default
// implementation that emits the Allow header is used.
func (r *Router) SetMethodNotAllowed(h http.Handler) {
	r.methodNotAll = h
}

// Handle registers an exact-path route for the given method.
// Per-route middleware wraps the handler before the default chain.
func (r *Router) Handle(method, path string, handler http.Handler, mws ...Middleware) {
	r.register(method, path, false, handler, mws)
}

// HandleFunc is a convenience for Handle with an http.HandlerFunc.
func (r *Router) HandleFunc(method, path string, handler http.HandlerFunc, mws ...Middleware) {
	r.register(method, path, false, handler, mws)
}

// HandlePrefix registers a longest-prefix route. When multiple prefix
// routes match a request, the one with the longest path wins.
func (r *Router) HandlePrefix(method, path string, handler http.Handler, mws ...Middleware) {
	r.register(method, path, true, handler, mws)
}

func (r *Router) register(method, path string, prefix bool, handler http.Handler, mws []Middleware) {
	if method == "" {
		panic("httpx: empty method")
	}
	if path == "" || path[0] != '/' {
		panic("httpx: path must start with /")
	}
	wrapped := r.wrap(handler, mws)
	if prefix {
		r.prefix = append(r.prefix, prefixRoute{method: method, path: path, handler: wrapped})
		sort.SliceStable(r.prefix, func(i, j int) bool {
			return len(r.prefix[i].path) > len(r.prefix[j].path)
		})
	} else {
		methods, ok := r.exact[path]
		if !ok {
			methods = make(map[string]http.Handler)
			r.exact[path] = methods
		}
		methods[method] = wrapped
	}
	allowed, ok := r.allowed[path]
	if !ok {
		allowed = make(map[string]struct{})
		r.allowed[path] = allowed
	}
	allowed[method] = struct{}{}
}

func (r *Router) wrap(handler http.Handler, mws []Middleware) http.Handler {
	chain := make([]Middleware, 0, len(r.defaultChain)+len(mws))
	chain = append(chain, r.defaultChain...)
	chain = append(chain, mws...)
	if len(chain) == 0 {
		return handler
	}
	return Chain(chain...)(handler)
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	if methods, ok := r.exact[path]; ok {
		if h, ok := methods[req.Method]; ok {
			h.ServeHTTP(w, req)
			return
		}
		r.writeMethodNotAllowed(w, req, path)
		return
	}
	for i := range r.prefix {
		pr := r.prefix[i]
		if !strings.HasPrefix(path, pr.path) {
			continue
		}
		if pr.method != req.Method {
			r.writeMethodNotAllowed(w, req, pr.path)
			return
		}
		pr.handler.ServeHTTP(w, req)
		return
	}
	if r.notFound != nil {
		r.notFound.ServeHTTP(w, req)
		return
	}
	http.NotFound(w, req)
}

func (r *Router) writeMethodNotAllowed(w http.ResponseWriter, req *http.Request, path string) {
	allow := r.allowHeader(path)
	if r.methodNotAll != nil {
		if allow != "" {
			w.Header().Set("Allow", allow)
		}
		r.methodNotAll.ServeHTTP(w, req)
		return
	}
	if allow != "" {
		w.Header().Set("Allow", allow)
	}
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

func (r *Router) allowHeader(path string) string {
	methods, ok := r.allowed[path]
	if !ok || len(methods) == 0 {
		return ""
	}
	out := make([]string, 0, len(methods))
	for m := range methods {
		out = append(out, m)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
