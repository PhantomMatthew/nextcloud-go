package wopi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func postContentsOverride(t *testing.T, env *wopiEnv, id int64, token string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		filesURL(id, true)+"?access_token="+token, strings.NewReader(body))
	req.Header.Set("X-WOPI-Override", "PUT_RELATIVE")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return env.serveCallback(req)
}

func postFileOp(t *testing.T, env *wopiEnv, id int64, token, override string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		filesURL(id, false)+"?access_token="+token, nil)
	req.Header.Set("X-WOPI-Override", override)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return env.serveCallback(req)
}

func TestPutRelativeFileExact(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, _, err := env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/doc.odt")
	m := env.mintToken(t, "alice", id)

	rr := postContentsOverride(t, env, id, m.Token,
		map[string]string{"X-WOPI-RelativeTarget": "doc-copy.odt"}, "copied bytes")
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT_RELATIVE = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["Name"] != "doc-copy.odt" {
		t.Errorf("Name = %q", resp["Name"])
	}
	newID := env.fileID(t, "/doc-copy.odt")
	if !strings.HasSuffix(resp["Url"], WopiFilesPrefix+strconv.FormatInt(newID, 10)) {
		t.Errorf("Url = %q, want the new file's WOPI url", resp["Url"])
	}
	if !strings.Contains(resp["HostViewUrl"], "fileId="+strconv.FormatInt(newID, 10)) ||
		resp["HostEditUrl"] != resp["HostViewUrl"] {
		t.Errorf("host urls = %q %q", resp["HostViewUrl"], resp["HostEditUrl"])
	}
	rc, _, err := env.dav.Read(ctx, "alice", "/doc-copy.odt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 64)
	n, _ := rc.Read(buf)
	if string(buf[:n]) != "copied bytes" {
		t.Errorf("new file content = %q", buf[:n])
	}

	// Conflict without the overwrite flag; allowed with it.
	rr = postContentsOverride(t, env, id, m.Token,
		map[string]string{"X-WOPI-RelativeTarget": "doc-copy.odt"}, "x")
	if rr.Code != http.StatusConflict {
		t.Fatalf("PUT_RELATIVE conflict = %d, want 409", rr.Code)
	}
	rr = postContentsOverride(t, env, id, m.Token,
		map[string]string{"X-WOPI-RelativeTarget": "doc-copy.odt", "X-WOPI-OverwriteRelativeTarget": "true"}, "v2")
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT_RELATIVE overwrite = %d body=%s", rr.Code, rr.Body.String())
	}
	rc2, _, err := env.dav.Read(ctx, "alice", "/doc-copy.odt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc2.Close()
	n, _ = rc2.Read(buf)
	if string(buf[:n]) != "v2" {
		t.Errorf("overwritten content = %q", buf[:n])
	}
}

func TestPutRelativeFileSuggestedDedupes(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, _, err := env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/doc.odt")
	m := env.mintToken(t, "alice", id)

	// A bare extension suggestion keeps the current base name and dedupes.
	rr := postContentsOverride(t, env, id, m.Token,
		map[string]string{"X-WOPI-SuggestedTarget": ".odt"}, "one")
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT_RELATIVE suggested = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["Name"] != "doc 1.odt" {
		t.Errorf("deduped name = %q, want %q", resp["Name"], "doc 1.odt")
	}
	if _, err := env.dav.Stat(ctx, "alice", "/doc 1.odt"); err != nil {
		t.Fatal(err)
	}

	// Header validation: both, neither, and a path-escaping name are 400.
	for _, headers := range []map[string]string{
		{"X-WOPI-RelativeTarget": "a.odt", "X-WOPI-SuggestedTarget": "b.odt"},
		{},
		{"X-WOPI-RelativeTarget": "sub/a.odt"},
		{"X-WOPI-RelativeTarget": ".."},
	} {
		if rr := postContentsOverride(t, env, id, m.Token, headers, "x"); rr.Code != http.StatusBadRequest {
			t.Errorf("headers %v = %d, want 400", headers, rr.Code)
		}
	}
}

func TestPutRelativeFileReadOnlyForbidden(t *testing.T) {
	env := newEnv(t)
	testShareeSetup(t, env)
	id := env.fileID(t, "/shared/report.odt")
	m := env.mintToken(t, "bob", id)
	if rr := postContentsOverride(t, env, id, m.Token,
		map[string]string{"X-WOPI-RelativeTarget": "evil.odt"}, "x"); rr.Code != http.StatusForbidden {
		t.Fatalf("bob PUT_RELATIVE = %d, want 403", rr.Code)
	}
}

func TestRenameFileFlow(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, _, err := env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/doc.odt")
	m := env.mintToken(t, "alice", id)

	// Lock, then rename: the wrong lock id conflicts, the held one renames.
	if rr := postFileOp(t, env, id, m.Token, "LOCK", map[string]string{"X-WOPI-Lock": "wopi-rn"}); rr.Code != http.StatusOK {
		t.Fatal("LOCK failed")
	}
	rr := postFileOp(t, env, id, m.Token, "RENAME_FILE",
		map[string]string{"X-WOPI-RequestedName": "renamed.odt", "X-WOPI-Lock": "wrong"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("RENAME_FILE wrong lock = %d, want 409", rr.Code)
	}
	rr = postFileOp(t, env, id, m.Token, "RENAME_FILE",
		map[string]string{"X-WOPI-RequestedName": "renamed.odt", "X-WOPI-Lock": "wopi-rn"})
	if rr.Code != http.StatusOK {
		t.Fatalf("RENAME_FILE = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["Name"] != "renamed.odt" || !strings.HasSuffix(resp["Url"], WopiFilesPrefix+strconv.FormatInt(id, 10)) {
		t.Errorf("rename response = %v", resp)
	}
	// The filecache id is stable across the rename: the same token resolves
	// to the new name.
	if _, err := env.dav.Stat(ctx, "alice", "/doc.odt"); err == nil {
		t.Error("old path still present after rename")
	}
	rr = env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet,
		filesURL(id, false)+"?access_token="+m.Token, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("CheckFileInfo after rename = %d", rr.Code)
	}
	var info fileInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.BaseFileName != "renamed.odt" {
		t.Errorf("BaseFileName after rename = %q", info.BaseFileName)
	}

	// Header validation.
	for _, headers := range []map[string]string{
		{},
		{"X-WOPI-RequestedName": "a/b.odt"},
	} {
		if rr := postFileOp(t, env, id, m.Token, "RENAME_FILE", headers); rr.Code != http.StatusBadRequest {
			t.Errorf("headers %v = %d, want 400", headers, rr.Code)
		}
	}
}

func TestRenameFileReadOnlyForbidden(t *testing.T) {
	env := newEnv(t)
	testShareeSetup(t, env)
	id := env.fileID(t, "/shared/report.odt")
	m := env.mintToken(t, "bob", id)
	if rr := postFileOp(t, env, id, m.Token, "RENAME_FILE",
		map[string]string{"X-WOPI-RequestedName": "evil.odt"}); rr.Code != http.StatusForbidden {
		t.Fatalf("bob RENAME_FILE = %d, want 403", rr.Code)
	}
}
