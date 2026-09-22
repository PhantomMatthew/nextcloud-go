package httpx

import (
	"net/http"
	"sort"
	"strings"
	"sync"
)

// MethodAny is a sentinel method matching any HTTP verb on a prefix
// route. Use it for protocols (e.g. WebDAV) where a single handler
// dispatches multiple methods internally.
const MethodAny = "*"

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
//
// Registration and removal are safe to run concurrently with serving
// (plugin hot-reload, ADR-0062): ServeHTTP resolves the handler under a
// single read lock and invokes it after unlocking, so removing a route
// never disturbs a request already dispatched to its handler.
type Router struct {
	defaultChain []Middleware

	mu           sync.RWMutex
	notFound     http.Handler
	methodNotAll http.Handler
	exact        map[string]map[string]http.Handler
	prefix       []prefixRoute
	allowed      map[string]map[string]struct{}
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
// Boot-time configuration; do not call concurrently with serving.
func (r *Router) SetNotFound(h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notFound = h
}

// SetMethodNotAllowed overrides the 405 handler. If unset, a default
// implementation that emits the Allow header is used. Boot-time
// configuration; do not call concurrently with serving.
func (r *Router) SetMethodNotAllowed(h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
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

// Remove unregisters one exact route (the only kind plugin hot-reload
// mounts). Unknown method/path pairs — and prefix routes — are untouched
// no-ops. The path's 405 Allow bookkeeping drops the method with it. A
// request already dispatched to the removed handler runs to completion;
// removal only stops new matches.
func (r *Router) Remove(method, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	methods, ok := r.exact[path]
	if !ok {
		return
	}
	if _, ok := methods[method]; !ok {
		return
	}
	delete(methods, method)
	if len(methods) == 0 {
		delete(r.exact, path)
	}
	if allowed, ok := r.allowed[path]; ok {
		delete(allowed, method)
		if len(allowed) == 0 {
			delete(r.allowed, path)
		}
	}
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

// ServeHTTP implements http.Handler. One read lock resolves the handler
// (or the 405 Allow header) and snapshots the fallback handlers; the
// handler itself runs unlocked so a slow plugin route never blocks
// registration, and so register/Remove (write lock) can proceed between
// requests. Go's sync.RWMutex forbids recursive read locking, so the
// Allow computation lives in the locked section instead of a helper that
// re-acquires it.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	var handler http.Handler
	var allow string
	notAllowed := false
	path := req.URL.Path
	if methods, ok := r.exact[path]; ok {
		if h, ok := methods[req.Method]; ok {
			handler = h
		} else {
			allow = r.allowHeader(path)
			notAllowed = true
		}
	} else {
		for i := range r.prefix {
			pr := r.prefix[i]
			if !strings.HasPrefix(path, pr.path) {
				continue
			}
			if pr.method != MethodAny && pr.method != req.Method {
				allow = r.allowHeader(pr.path)
				notAllowed = true
			} else {
				handler = pr.handler
			}
			break
		}
	}
	notFound := r.notFound
	methodNotAll := r.methodNotAll
	r.mu.RUnlock()

	switch {
	case handler != nil:
		handler.ServeHTTP(w, req)
	case notAllowed:
		writeMethodNotAllowed(w, req, methodNotAll, allow)
	case notFound != nil:
		notFound.ServeHTTP(w, req)
	default:
		http.NotFound(w, req)
	}
}

func writeMethodNotAllowed(w http.ResponseWriter, req *http.Request, custom http.Handler, allow string) {
	if allow != "" {
		w.Header().Set("Allow", allow)
	}
	if custom != nil {
		custom.ServeHTTP(w, req)
		return
	}
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

// allowHeader renders the sorted Allow header for path. Callers hold
// r.mu (read).
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
