package sharing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
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

func TestShareesRecommendedEmpty(t *testing.T) {
	h := ShareesHandler{Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees/recommended?format=json", nil)
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data, _ := env["ocs"].(map[string]any)["data"].(map[string]any)
	if users, _ := data["users"].([]any); len(users) != 0 {
		t.Fatalf("users = %v", users)
	}
}

func TestShareesRecommended(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	us := users.NewSQLStore(db)
	admin := &users.User{UID: "admin", DisplayName: "Admin", PasswordHash: "x", Enabled: true}
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	for _, u := range []*users.User{admin, bob} {
		if err := us.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := us.CreateGroup(ctx, &users.Group{GID: "engineers", DisplayName: "Engineers"}); err != nil {
		t.Fatal(err)
	}
	store := NewSQLShareStore(db)
	for _, sh := range []*files.Share{
		{OwnerUserID: admin.ID, ShareType: files.ShareTypeUser, Path: "/a.txt", Token: "tok-a", ShareWith: "bob", StimeMs: 100},
		{OwnerUserID: admin.ID, ShareType: files.ShareTypeGroup, Path: "/b.txt", Token: "tok-b", ShareWith: "engineers", StimeMs: 200},
		{OwnerUserID: admin.ID, ShareType: files.ShareTypeUser, Path: "/c.txt", Token: "tok-c", ShareWith: "bob", StimeMs: 300},
	} {
		if err := store.Insert(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	h := ShareesHandler{Version: ocs.V2, Users: us, Shares: store}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/ocs/v2.php/apps/files_sharing/api/v1/sharees/recommended?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "admin"}))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data, _ := env["ocs"].(map[string]any)["data"].(map[string]any)
	usersList, _ := data["users"].([]any)
	groupsList, _ := data["groups"].([]any)
	if len(usersList) != 1 || len(groupsList) != 1 {
		t.Fatalf("users=%v groups=%v", usersList, groupsList)
	}
	u0, _ := usersList[0].(map[string]any)
	if u0["label"] != "Bob" {
		t.Fatalf("user label = %v", u0["label"])
	}
	g0, _ := groupsList[0].(map[string]any)
	gv, _ := g0["value"].(map[string]any)
	if gv["shareType"] != float64(1) || gv["shareWith"] != "engineers" {
		t.Fatalf("group value = %v", gv)
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
