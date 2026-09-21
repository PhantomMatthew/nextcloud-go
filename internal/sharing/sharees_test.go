package sharing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

func TestShareesExactRemote(t *testing.T) {
	h := ShareesHandler{Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees?search=bob@https://remote.example.com&itemType=file&format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	remotes := shareesExactRemotes(t, rr.Body.Bytes())
	if len(remotes) != 1 {
		t.Fatalf("remotes = %v", remotes)
	}
	if remotes[0]["label"] != "bob@https://remote.example.com" {
		t.Fatalf("label = %v", remotes[0]["label"])
	}
	val, _ := remotes[0]["value"].(map[string]any)
	if val["shareType"] != float64(6) || val["shareWith"] != "bob@https://remote.example.com" || val["server"] != "https://remote.example.com" {
		t.Fatalf("value = %v", val)
	}
}

func TestShareesNoAtEmpty(t *testing.T) {
	h := ShareesHandler{Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees?search=bob&itemType=file&format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if remotes := shareesExactRemotes(t, rr.Body.Bytes()); len(remotes) != 0 {
		t.Fatalf("remotes = %v", remotes)
	}
}

func TestShareesSelfHostEmpty(t *testing.T) {
	h := ShareesHandler{Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees?search=admin@http://cloud.example.com&itemType=file&format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if remotes := shareesExactRemotes(t, rr.Body.Bytes()); len(remotes) != 0 {
		t.Fatalf("remotes = %v", remotes)
	}
}

func TestShareesRecommendedNotFound(t *testing.T) {
	h := ShareesHandler{Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees/recommended?format=json", nil)
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func shareesExactRemotes(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	data, _ := env["ocs"].(map[string]any)["data"].(map[string]any)
	exact, _ := data["exact"].(map[string]any)
	raw, _ := exact["remotes"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		out = append(out, m)
	}
	return out
}
