package sharing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

func TestLookupSearchHit(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("search")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"federationId":"bob@https://remote.example.com","name":"Bob"},{"federationId":"","name":"Skip"}]`))
	}))
	defer srv.Close()
	c := &LookupClient{BaseURL: srv.URL}
	hits, err := c.Search(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "bob" {
		t.Fatalf("search param = %q", gotQuery)
	}
	if len(hits) != 1 || hits[0].FederationID != "bob@https://remote.example.com" || hits[0].Name != "Bob" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestLookupSearchBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &LookupClient{BaseURL: srv.URL}
	if _, err := c.Search(t.Context(), "bob"); err == nil {
		t.Fatal("expected error")
	}
}

func TestLookupSearchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	c := &LookupClient{BaseURL: srv.URL, HTTP: &http.Client{Timeout: 20 * time.Millisecond}}
	if _, err := c.Search(t.Context(), "bob"); err == nil {
		t.Fatal("expected error")
	}
}

func TestLookupSearchDisabled(t *testing.T) {
	var nilClient *LookupClient
	if hits, err := nilClient.Search(t.Context(), "bob"); err != nil || hits != nil {
		t.Fatalf("nil client: hits=%v err=%v", hits, err)
	}
	c := &LookupClient{BaseURL: ""}
	if hits, err := c.Search(t.Context(), "bob"); err != nil || hits != nil {
		t.Fatalf("empty BaseURL: hits=%v err=%v", hits, err)
	}
}

func TestShareesLookupHit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"federationId":"bob@https://remote.example.com","name":"Bob Remote"}]`))
	}))
	defer srv.Close()
	h := ShareesHandler{Version: ocs.V2, Lookup: &LookupClient{BaseURL: srv.URL}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees?search=bob&lookup=true&itemType=file&format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	lookup := shareesLookup(t, rr.Body.Bytes())
	if len(lookup) != 1 {
		t.Fatalf("lookup = %v", lookup)
	}
	if lookup[0]["label"] != "Bob Remote" {
		t.Fatalf("label = %v", lookup[0]["label"])
	}
	val, _ := lookup[0]["value"].(map[string]any)
	if val["shareType"] != float64(6) || val["shareWith"] != "bob@https://remote.example.com" || val["server"] != "https://remote.example.com" {
		t.Fatalf("value = %v", val)
	}
}

func TestShareesLookupNotRequested(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	h := ShareesHandler{Version: ocs.V2, Lookup: &LookupClient{BaseURL: srv.URL}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees?search=bob&format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("lookup server called without lookup=true")
	}
	if lookup := shareesLookup(t, rr.Body.Bytes()); len(lookup) != 0 {
		t.Fatalf("lookup = %v", lookup)
	}
}

func TestShareesLookupErrorSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	h := ShareesHandler{Version: ocs.V2, Lookup: &LookupClient{BaseURL: srv.URL}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees?search=bob&lookup=true&format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if lookup := shareesLookup(t, rr.Body.Bytes()); len(lookup) != 0 {
		t.Fatalf("lookup = %v", lookup)
	}
}

func TestShareesLookupSelfHostFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"federationId":"admin@http://cloud.example.com","name":"Admin"}]`))
	}))
	defer srv.Close()
	h := ShareesHandler{Version: ocs.V2, Lookup: &LookupClient{BaseURL: srv.URL}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees?search=admin&lookup=true&format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if lookup := shareesLookup(t, rr.Body.Bytes()); len(lookup) != 0 {
		t.Fatalf("lookup = %v", lookup)
	}
}

func shareesLookup(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	data, _ := env["ocs"].(map[string]any)["data"].(map[string]any)
	raw, _ := data["lookup"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		out = append(out, m)
	}
	return out
}
