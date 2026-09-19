package files

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func TestLockHandlerPutDelete(t *testing.T) {
	dav := newLockDAV(t)
	ctx := t.Context()
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	ent, err := dav.Stat(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatUint(ent.NumericID, 10)
	h := LockHandler{DAV: dav, Version: ocs.V2}

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPut, "/ocs/v2.php/apps/files_lock/lock/"+id+"?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "alice", DisplayName: "Alice", Enabled: true}))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("lock status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data := env["ocs"].(map[string]any)["data"].(map[string]any)
	token, _ := data["token"].(string)
	if token == "" || data["lock"] != true {
		t.Fatalf("payload = %v", data)
	}
	if err := dav.CheckLock(ctx, "alice", "/a.txt", ""); !errors.Is(err, webdav.ErrLocked) {
		t.Fatalf("check = %v", err)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodDelete, "/ocs/v2.php/apps/files_lock/lock/"+id+"?format=json&token="+token, nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "alice", DisplayName: "Alice", Enabled: true}))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unlock status = %d body=%s", rr.Code, rr.Body.String())
	}
	if err := dav.CheckLock(ctx, "alice", "/a.txt", ""); err != nil {
		t.Fatalf("after ocs unlock = %v", err)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodPut, "/ocs/v2.php/apps/files_lock/lock/999?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "alice", Enabled: true}))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing fileid status = %d", rr.Code)
	}
}
