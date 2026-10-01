package wopi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// wopiEnv is the scheme-0 (plaintext) e2e harness: DAV + users + the sharing
// service for sharee cases, mirroring internal/sharing/ocs_test.go's
// testService construction, plus a wired Versions store so PutFile snapshots
// are observable.
type wopiEnv struct {
	dav       *files.DAV
	us        *users.SQLStore
	svc       *Service
	store     *SQLStore
	shares    *sharing.Service
	versions  files.VersionStore
	mint      *MintHandler
	callbacks *FilesHandler
	freeze    time.Time
}

func newEnv(t *testing.T) *wopiEnv {
	t.Helper()
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	for _, u := range []struct{ uid, name string }{{"alice", "Alice"}, {"bob", "Bob"}, {"carol", "Carol"}} {
		if err := us.Create(ctx, &users.User{UID: u.uid, DisplayName: u.name, PasswordHash: "x", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := files.NewDAV(st, files.NewSQLStore(db), us)
	freeze := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return freeze }
	dav.Locks = files.NewSQLLockStore(db)
	shareStore := sharing.NewSQLShareStore(db)
	dav.Shares = shareStore
	versionStore := files.NewSQLVersionStore(db)
	ver := files.NewVersions(st, versionStore, dav, us)
	ver.Clock = dav.Clock
	dav.Versions = ver
	shares := &sharing.Service{Store: shareStore, Files: dav, Users: us, Clock: dav.Clock}
	dav.Incoming = files.MultiIncoming{shares}
	tokStore := NewSQLStore(db)
	svc := &Service{Store: tokStore, Files: dav, Users: us, Shares: shares, TTL: time.Hour, Clock: dav.Clock}
	return &wopiEnv{
		dav:       dav,
		us:        us,
		svc:       svc,
		store:     tokStore,
		shares:    shares,
		versions:  versionStore,
		mint:      &MintHandler{Svc: svc},
		callbacks: &FilesHandler{Svc: svc},
		freeze:    freeze,
	}
}

func withUser(r *http.Request, uid, name string) *http.Request {
	return r.WithContext(auth.WithUser(r.Context(), &auth.Principal{UID: uid, DisplayName: name, Enabled: true}))
}

// fileID resolves alice's filecache id for p (every e2e document is hers;
// sharees reach it through their mounts).
func (e *wopiEnv) fileID(t *testing.T, p string) int64 {
	t.Helper()
	u, err := e.us.GetByUID(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	row, err := e.dav.Meta.GetByPath(t.Context(), u.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func (e *wopiEnv) mintToken(t *testing.T, uid string, id int64) mintResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/index.php/apps/richdocuments/wopi/token?fileId="+strconv.FormatInt(id, 10), nil)
	e.mint.ServeHTTP(rr, withUser(req, uid, uid))
	if rr.Code != http.StatusOK {
		t.Fatalf("mint as %s: status = %d body=%s", uid, rr.Code, rr.Body.String())
	}
	var out mintResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *wopiEnv) serveCallback(r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	e.callbacks.ServeHTTP(rr, r)
	return rr
}

func filesURL(id int64, contents bool) string {
	u := WopiFilesPrefix + strconv.FormatInt(id, 10)
	if contents {
		u += "/contents"
	}
	return u
}

func TestMintAsOwner(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, _, err := env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("hello wopi"), nil); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/doc.odt")
	m := env.mintToken(t, "alice", id)
	if m.Token == "" {
		t.Fatal("empty token")
	}
	if !m.CanWrite {
		t.Error("can_write = false, want true for owner")
	}
	wantSrc := "http://example.com" + filesURL(id, false)
	if m.WopiSrc != wantSrc {
		t.Errorf("wopi_src = %q, want %q", m.WopiSrc, wantSrc)
	}
	if m.ExpiresAt != env.freeze.Add(time.Hour).UnixMilli() {
		t.Errorf("expires_at = %d, want %d", m.ExpiresAt, env.freeze.Add(time.Hour).UnixMilli())
	}

	// Bad fileId values are 400; unknown and unshared ids are 404.
	for _, q := range []string{"", "abc", "0", "-3"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/apps/richdocuments/wopi/token?fileId="+q, nil)
		env.mint.ServeHTTP(rr, withUser(req, "alice", "Alice"))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("fileId=%q: status = %d, want 400", q, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/apps/richdocuments/wopi/token?fileId=99999", nil)
	env.mint.ServeHTTP(rr, withUser(req, "alice", "Alice"))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", rr.Code)
	}
}

func TestCheckFileInfoAndGetFile(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, _, err := env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("hello wopi"), nil); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/doc.odt")
	m := env.mintToken(t, "alice", id)

	rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, filesURL(id, false)+"?access_token="+m.Token, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("CheckFileInfo status = %d body=%s", rr.Code, rr.Body.String())
	}
	var info fileInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.BaseFileName != "doc.odt" {
		t.Errorf("BaseFileName = %q", info.BaseFileName)
	}
	if info.Size != int64(len("hello wopi")) {
		t.Errorf("Size = %d", info.Size)
	}
	if info.OwnerID != "alice" || info.UserID != "alice" {
		t.Errorf("OwnerId/UserId = %q/%q", info.OwnerID, info.UserID)
	}
	if info.UserFriendlyName != "Alice" {
		t.Errorf("UserFriendlyName = %q", info.UserFriendlyName)
	}
	if !info.UserCanWrite {
		t.Error("UserCanWrite = false")
	}
	if info.Version == "" {
		t.Error("Version empty")
	}
	if info.LastModifiedTime == "" {
		t.Error("LastModifiedTime empty")
	}

	// GetFile returns the source bytes exactly.
	rr = env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, filesURL(id, true)+"?access_token="+m.Token, nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "hello wopi" {
		t.Fatalf("GetFile = %d %q", rr.Code, rr.Body.String())
	}
}

func TestWOPILockPutUnlockFlow(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, _, err := env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/doc.odt")
	m := env.mintToken(t, "alice", id)
	q := "?access_token=" + m.Token

	post := func(url, override, lockID, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
		if override != "" {
			req.Header.Set("X-WOPI-Override", override)
		}
		if lockID != "" {
			req.Header.Set("X-WOPI-Lock", lockID)
		}
		return env.serveCallback(req)
	}

	// LOCK, then a PutFile with the wrong lock id conflicts; with the held
	// id it writes (and snapshots the previous bytes as a version).
	if rr := post(filesURL(id, false)+q, "LOCK", "wopi-abc", ""); rr.Code != http.StatusOK {
		t.Fatalf("LOCK = %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := post(filesURL(id, true)+q, "", "wrong", "v2"); rr.Code != http.StatusConflict {
		t.Fatalf("PutFile wrong lock = %d, want 409", rr.Code)
	}
	if rr := post(filesURL(id, true)+q, "", "wopi-abc", "v2"); rr.Code != http.StatusOK {
		t.Fatalf("PutFile held lock = %d body=%s", rr.Code, rr.Body.String())
	}
	rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, filesURL(id, true)+q, nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "v2" {
		t.Fatalf("GetFile after put = %d %q", rr.Code, rr.Body.String())
	}
	alice, err := env.us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	vers, err := env.versions.ListByPath(ctx, alice.ID, "/doc.odt")
	if err != nil || len(vers) != 1 {
		t.Fatalf("versions = %d %v, want exactly 1 snapshot", len(vers), err)
	}

	// UNLOCK mismatch conflicts; the matching id releases. An unlocked file
	// accepts PutFile with no lock header (WOPI allows it).
	if rr := post(filesURL(id, false)+q, "UNLOCK", "wopi-wrong", ""); rr.Code != http.StatusConflict {
		t.Fatalf("UNLOCK mismatch = %d, want 409", rr.Code)
	}
	if rr := post(filesURL(id, false)+q, "UNLOCK", "wopi-abc", ""); rr.Code != http.StatusOK {
		t.Fatalf("UNLOCK = %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := post(filesURL(id, true)+q, "", "", "v3"); rr.Code != http.StatusOK {
		t.Fatalf("PutFile unlocked = %d body=%s", rr.Code, rr.Body.String())
	}

	// REFRESH_LOCK refreshes a matching lock, conflicts otherwise.
	if rr := post(filesURL(id, false)+q, "LOCK", "wopi-abc", ""); rr.Code != http.StatusOK {
		t.Fatalf("re-LOCK = %d", rr.Code)
	}
	if rr := post(filesURL(id, false)+q, "REFRESH_LOCK", "wopi-abc", ""); rr.Code != http.StatusOK {
		t.Fatalf("REFRESH_LOCK match = %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := post(filesURL(id, false)+q, "REFRESH_LOCK", "wopi-other", ""); rr.Code != http.StatusConflict {
		t.Fatalf("REFRESH_LOCK mismatch = %d, want 409", rr.Code)
	}

	// LOCK without the X-WOPI-Lock header is 400; an unknown override is 501.
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, filesURL(id, false)+q, nil)
	req.Header.Set("X-WOPI-Override", "LOCK")
	if rr := env.serveCallback(req); rr.Code != http.StatusBadRequest {
		t.Fatalf("LOCK without header = %d, want 400", rr.Code)
	}
	if rr := post(filesURL(id, false)+q, "PUT_RELATIVE", "wopi-abc", ""); rr.Code != http.StatusNotImplemented {
		t.Fatalf("unknown override = %d, want 501", rr.Code)
	}
}

func TestWOPITokenAuthFailures(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, _, err := env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/doc.odt")

	// A bad token is 401 on every endpoint shape; a missing token is 401.
	for _, u := range []string{
		filesURL(id, false) + "?access_token=nope",
		filesURL(id, true) + "?access_token=nope",
		filesURL(id, false),
		filesURL(id, true),
	} {
		rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, u, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s: status = %d, want 401", u, rr.Code)
		}
	}
	rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodPost, filesURL(id, true)+"?access_token=nope", strings.NewReader("x")))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("POST contents bad token: status = %d, want 401", rr.Code)
	}

	// An expired token is 401 even though the row exists.
	if err := env.store.Insert(ctx, &Token{Token: "tok-expired", UID: "alice", FileID: id, CanWrite: true, ExpiresAt: env.freeze.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	rr = env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, filesURL(id, false)+"?access_token=tok-expired", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expired token: status = %d, want 401", rr.Code)
	}

	// A valid token minted for a DIFFERENT file id is 401 here.
	if _, _, err := env.dav.Write(ctx, "alice", "/other.odt", strings.NewReader("o"), nil); err != nil {
		t.Fatal(err)
	}
	otherID := env.fileID(t, "/other.odt")
	m := env.mintToken(t, "alice", otherID)
	rr = env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, filesURL(id, false)+"?access_token="+m.Token, nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("cross-file token: status = %d, want 401", rr.Code)
	}
}

func TestWOPIShareeAccess(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, err := env.dav.Mkdir(ctx, "alice", "/shared"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(ctx, "alice", "/shared/report.odt", strings.NewReader("shared bytes"), nil); err != nil {
		t.Fatal(err)
	}
	alice, err := env.us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// Read-only user share of /shared to bob (insertShare pattern: a direct
	// row, bypassing the notify paths the OCS handler would fire).
	if err := env.shares.Store.Insert(ctx, &files.Share{
		OwnerUserID: alice.ID, ShareType: files.ShareTypeUser, Path: "/shared",
		ItemType: "folder", Token: "wopishare0000001", Permissions: webdav.PermRead, ShareWith: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/shared/report.odt")

	// Bob mints: the share grants no update, so can_write is false, and the
	// owner still shows as OwnerId in CheckFileInfo.
	m := env.mintToken(t, "bob", id)
	if m.CanWrite {
		t.Error("bob can_write = true, want false for a read-only share")
	}
	q := "?access_token=" + m.Token
	rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, filesURL(id, false)+q, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("bob CheckFileInfo = %d body=%s", rr.Code, rr.Body.String())
	}
	var info fileInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.OwnerID != "alice" || info.UserID != "bob" || info.UserCanWrite {
		t.Errorf("bob CheckFileInfo = %+v", info)
	}

	// Reads flow through the mount; writes and locks are 403.
	rr = env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, filesURL(id, true)+q, nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "shared bytes" {
		t.Fatalf("bob GetFile = %d %q", rr.Code, rr.Body.String())
	}
	rr = env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodPost, filesURL(id, true)+q, strings.NewReader("x")))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bob PutFile = %d, want 403", rr.Code)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, filesURL(id, false)+q, nil)
	req.Header.Set("X-WOPI-Override", "LOCK")
	req.Header.Set("X-WOPI-Lock", "wopi-bob")
	if rr := env.serveCallback(req); rr.Code != http.StatusForbidden {
		t.Fatalf("bob LOCK = %d, want 403", rr.Code)
	}
	// A read-only token must not release or refresh another editor's lock
	// either (the lock id is bearer-shaped, not a capability).
	for _, override := range []string{"UNLOCK", "REFRESH_LOCK"} {
		req = httptest.NewRequestWithContext(ctx, http.MethodPost, filesURL(id, false)+q, nil)
		req.Header.Set("X-WOPI-Override", override)
		req.Header.Set("X-WOPI-Lock", "wopi-bob")
		if rr := env.serveCallback(req); rr.Code != http.StatusForbidden {
			t.Fatalf("bob %s = %d, want 403", override, rr.Code)
		}
	}

	// Carol has no share: minting is 404 (no existence oracle).
	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/apps/richdocuments/wopi/token?fileId="+strconv.FormatInt(id, 10), nil)
	env.mint.ServeHTTP(rr, withUser(req, "carol", "Carol"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("carol mint = %d, want 404", rr.Code)
	}
}

func TestWOPIDirFileID(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, err := env.dav.Mkdir(ctx, "alice", "/dir"); err != nil {
		t.Fatal(err)
	}
	dirID := env.fileID(t, "/dir")

	// Minting for a directory is a client error.
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/apps/richdocuments/wopi/token?fileId="+strconv.FormatInt(dirID, 10), nil)
	env.mint.ServeHTTP(rr, withUser(req, "alice", "Alice"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("dir mint = %d body=%s, want 400", rr.Code, rr.Body.String())
	}

	// A token for a dir id (mintable only by direct insert — mint rejects
	// dirs) gets a 404 from the callbacks: a dir has no WOPI document.
	if err := env.store.Insert(ctx, &Token{Token: "tok-dir", UID: "alice", FileID: dirID, CanWrite: true, ExpiresAt: env.freeze.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{filesURL(dirID, false) + "?access_token=tok-dir", filesURL(dirID, true) + "?access_token=tok-dir"} {
		rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet, u, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", u, rr.Code)
		}
	}
}
