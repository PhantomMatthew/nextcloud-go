package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func tagMW(name string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("X-Chain", name)
			next.ServeHTTP(w, r)
		})
	}
}

func TestRouterExactMatch(t *testing.T) {
	r := NewRouter()
	r.Handle(http.MethodGet, "/status.php", okHandler("status"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/status.php", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "status" {
		t.Fatalf("exact: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestRouterNotFound(t *testing.T) {
	r := NewRouter()
	r.Handle(http.MethodGet, "/status.php", okHandler("status"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/missing", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("404: got %d", rr.Code)
	}
}

func TestRouterMethodNotAllowed(t *testing.T) {
	r := NewRouter()
	r.Handle(http.MethodGet, "/x", okHandler("x"))
	r.Handle(http.MethodPost, "/x", okHandler("x"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/x", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("405: got %d", rr.Code)
	}
	allow := rr.Header().Get("Allow")
	if allow != "GET, POST" {
		t.Errorf("Allow: got %q want %q", allow, "GET, POST")
	}
}

func TestRouterPrefixLongestWins(t *testing.T) {
	r := NewRouter()
	r.HandlePrefix(http.MethodGet, "/remote.php/", okHandler("short"))
	r.HandlePrefix(http.MethodGet, "/remote.php/dav/", okHandler("long"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/remote.php/dav/files/u", nil))
	if rr.Body.String() != "long" {
		t.Errorf("longest prefix: got %q want long", rr.Body.String())
	}
}

func TestRouterDefaultChainAndPerRoute(t *testing.T) {
	r := NewRouter(tagMW("base1"), tagMW("base2"))
	r.Handle(http.MethodGet, "/y", okHandler("y"), tagMW("route1"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/y", nil))
	chain := rr.Header().Values("X-Chain")
	want := []string{"base1", "base2", "route1"}
	if strings.Join(chain, ",") != strings.Join(want, ",") {
		t.Errorf("chain order: got %v want %v", chain, want)
	}
}

func TestRouterRouteWithoutDefaultChainBypassesIt(t *testing.T) {
	r := NewRouter(tagMW("base"))
	r.Handle(http.MethodGet, "/y", okHandler("y"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/y", nil))
	if got := rr.Header().Get("X-Chain"); got != "base" {
		t.Errorf("expected default chain to apply, got %q", got)
	}
}

func TestRouterPanicOnInvalidPath(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("expected panic on relative path")
		}
	}()
	NewRouter().Handle(http.MethodGet, "rel", okHandler("x"))
}

func TestRouterRemove(t *testing.T) {
	r := NewRouter()
	r.Handle(http.MethodGet, "/x", okHandler("x"))
	r.Handle(http.MethodPost, "/x", okHandler("x"))

	r.Remove(http.MethodGet, "/x")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" {
		t.Fatalf("after remove GET: code=%d allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST must survive: code=%d", rr.Code)
	}

	r.Remove(http.MethodPost, "/x")
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("after removing last method: code=%d, want 404", rr.Code)
	}
}

func TestRouterRemoveNoop(t *testing.T) {
	r := NewRouter()
	r.Handle(http.MethodGet, "/x", okHandler("x"))
	// Unknown path, unknown method, and prefix routes are all silent no-ops.
	r.Remove(http.MethodDelete, "/missing")
	r.Remove(http.MethodDelete, "/x")
	r.HandlePrefix(MethodAny, "/pre", okHandler("pre"))
	r.Remove(http.MethodGet, "/pre")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /x must survive no-op removals: code=%d", rr.Code)
	}
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/pre/a", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "pre" {
		t.Fatalf("prefix route must not be removable: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

// TestRouterConcurrentRegisterRemoveServe hammers ServeHTTP while another
// goroutine churns register/Remove on the same paths; it exists for the
// race detector (-race), not for a fixed outcome.
func TestRouterConcurrentRegisterRemoveServe(t *testing.T) {
	r := NewRouter()
	r.Handle(http.MethodGet, "/stable", okHandler("stable"))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				rr := httptest.NewRecorder()
				r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/hot", nil))
				r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/hot", nil))
				r.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/stable", nil))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			r.Handle(http.MethodGet, "/hot", okHandler("hot"))
			r.Remove(http.MethodGet, "/hot")
		}
	}()
	wg.Wait()
}
