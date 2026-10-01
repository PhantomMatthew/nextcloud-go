package files_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/activity"
	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// activityRow is one raw activities row for assertions (the at-rest view).
type activityRow struct {
	id         int64
	userID     int64
	actor      string
	app        string
	typ        string
	subject    string
	rich       string
	params     string
	objectType string
	objectID   int64
	objectName string
}

func activityRowsFor(t *testing.T, db database.DB, userID int64) []activityRow {
	t.Helper()
	rows, err := db.Query(context.Background(), `
SELECT id, user_id, actor_uid, app, type, subject, subject_rich, subject_rich_parameters,
       object_type, object_id, object_name
FROM activities WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []activityRow
	for rows.Next() {
		var r activityRow
		if err := rows.Scan(&r.id, &r.userID, &r.actor, &r.app, &r.typ, &r.subject, &r.rich,
			&r.params, &r.objectType, &r.objectID, &r.objectName); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// activityParam mirrors the producer's rich-object parameter shape.
type activityParam struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

func activityParams(t *testing.T, raw string) map[string]activityParam {
	t.Helper()
	pm := map[string]activityParam{}
	if err := json.Unmarshal([]byte(raw), &pm); err != nil {
		t.Fatalf("params %q: %v", raw, err)
	}
	return pm
}

// listActivityOCS serves the OCS activity list as the principal through a
// handler carrying the §9 subject decryptor, returning the decoded data
// array, the raw body, and the X-Activity-Last-Given header.
func listActivityOCS(t *testing.T, store activity.Store, us users.Store, subjects activity.SubjectDecryptor, p *auth.Principal) ([]any, string, string) {
	t.Helper()
	h := activity.Handler{Store: store, Users: us, Version: ocs.V2, Subjects: subjects}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/ocs/v2.php/apps/activity/api/v2/activity?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), p))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("activity list as %s = %d: %s", p.UID, rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data, ok := env["ocs"].(map[string]any)["data"].([]any)
	if !ok {
		t.Fatalf("data shape = %s", rr.Body.String())
	}
	return data, rr.Body.String(), rr.Header().Get("X-Activity-Last-Given")
}

// ocsItem extracts one OCS activity entry's fields for assertions.
func ocsItem(t *testing.T, item any) (subject, objectName string, params map[string]any) {
	t.Helper()
	m, ok := item.(map[string]any)
	if !ok {
		t.Fatalf("item = %v", item)
	}
	subject, _ = m["subject"].(string)
	objectName, _ = m["object_name"].(string)
	rich, ok := m["subject_rich"].([]any)
	if !ok || len(rich) != 2 {
		t.Fatalf("subject_rich = %v", m["subject_rich"])
	}
	params, _ = rich[1].(map[string]any)
	return subject, objectName, params
}

// assertNoSchemeMarker fails if any rendered param key is a §9 marker.
func assertNoSchemeMarker(t *testing.T, params map[string]any) {
	t.Helper()
	for key := range params {
		if key == "ncgoNameScheme" || strings.HasPrefix(key, "ncgoNameScheme:") {
			t.Errorf("rendered params still carry the marker %q: %v", key, params)
		}
	}
}

// filecacheTokens returns every stored name token of the user's tree
// (full values — leak assertions compare exact tokens, never substrings).
func filecacheTokens(t *testing.T, db database.DB, userID int64) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT name FROM files WHERE user_id = ?`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "" {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestActivityFileLifecycleScheme0 pins the scheme-0 producer end to end
// (ADR-0104 §9's first writer, plaintext tree): every DAV verb lands exactly
// one upstream-shaped row in the owner's stream, carries no marker, and the
// OCS list renders it verbatim with the upstream ordering contract.
func TestActivityFileLifecycleScheme0(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	store := activity.NewSQLStore(env.db)
	env.dav.Activity = store
	aliceID := env.ids["alice"]

	fileID := func(p string) int64 {
		t.Helper()
		f, err := env.meta.GetByPath(ctx, aliceID, p)
		if err != nil {
			t.Fatal(err)
		}
		return f.ID
	}
	// wantRow pins the newest row's exact shape after each verb; count is the
	// expected total (each verb adds exactly one row).
	wantRow := func(count int, typ, template, subject, objectName string, objectID int64, params map[string]activityParam) {
		t.Helper()
		rows := activityRowsFor(t, env.db, aliceID)
		if len(rows) != count {
			t.Fatalf("rows = %d, want %d: %+v", len(rows), count, rows)
		}
		r := rows[count-1]
		if r.userID != aliceID || r.actor != "alice" || r.app != "files" || r.typ != typ {
			t.Errorf("row meta = %+v, want type %s actor alice app files", r, typ)
		}
		if r.subject != subject || r.rich != template {
			t.Errorf("row subject = %q rich = %q, want %q / %q", r.subject, r.rich, subject, template)
		}
		if r.objectType != "files" || r.objectID != objectID || r.objectName != objectName {
			t.Errorf("row object = %s %d %q, want files %d %q", r.objectType, r.objectID, r.objectName, objectID, objectName)
		}
		got := activityParams(t, r.params)
		if len(got) != len(params) {
			t.Fatalf("row params = %v, want %v", got, params)
		}
		for k, want := range params {
			if got[k] != want {
				t.Errorf("param %q = %+v, want %+v", k, got[k], want)
			}
		}
		if strings.Contains(r.params, "ncgoNameScheme") {
			t.Errorf("scheme-0 row carries the marker: %q", r.params)
		}
	}
	actor := activityParam{Type: "user", ID: "alice", Name: "alice"}
	fileParam := func(id int64, name string) activityParam {
		return activityParam{Type: "file", ID: strconv.FormatInt(id, 10), Name: name}
	}

	// PUT create → file_created with the exact upstream shape.
	if _, _, err := env.dav.Write(ctx, "alice", "/put.txt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	putID := fileID("/put.txt")
	wantRow(1, "file_created", "{actor} created {file}", "alice created put.txt", "put.txt", putID,
		map[string]activityParam{"actor": actor, "file": fileParam(putID, "put.txt")})

	// PUT overwrite → file_changed.
	if _, _, err := env.dav.Write(ctx, "alice", "/put.txt", strings.NewReader("v2"), nil); err != nil {
		t.Fatal(err)
	}
	wantRow(2, "file_changed", "{actor} changed {file}", "alice changed put.txt", "put.txt", putID,
		map[string]activityParam{"actor": actor, "file": fileParam(putID, "put.txt")})

	// MKCOL → file_created for the folder.
	if _, err := env.dav.Mkdir(ctx, "alice", "/dir"); err != nil {
		t.Fatal(err)
	}
	dirID := fileID("/dir")
	wantRow(3, "file_created", "{actor} created {file}", "alice created dir", "dir", dirID,
		map[string]activityParam{"actor": actor, "file": fileParam(dirID, "dir")})

	// DELETE (trash path) → file_deleted with the plaintext basename.
	if err := env.dav.Remove(ctx, "alice", "/put.txt"); err != nil {
		t.Fatal(err)
	}
	wantRow(4, "file_deleted", "{actor} deleted {file}", "alice deleted put.txt", "put.txt", putID,
		map[string]activityParam{"actor": actor, "file": fileParam(putID, "put.txt")})

	// Same-directory MOVE → one file_renamed with oldfile + file plaintext.
	env.write(t, "/mv.txt", "m")
	mvID := fileID("/mv.txt")
	if _, _, err := env.dav.Move(ctx, "alice", "/mv.txt", "alice", "/mv2.txt", false); err != nil {
		t.Fatal(err)
	}
	wantRow(6, "file_renamed", "{actor} renamed {oldfile} to {file}", "alice renamed mv.txt to mv2.txt", "mv2.txt", mvID,
		map[string]activityParam{"actor": actor, "file": fileParam(mvID, "mv2.txt"), "oldfile": fileParam(mvID, "mv.txt")})

	// Cross-directory MOVE → the same shape.
	if _, err := env.dav.Mkdir(ctx, "alice", "/x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Move(ctx, "alice", "/mv2.txt", "alice", "/x/mv2.txt", false); err != nil {
		t.Fatal(err)
	}
	wantRow(8, "file_renamed", "{actor} renamed {oldfile} to {file}", "alice renamed mv2.txt to mv2.txt", "mv2.txt", mvID,
		map[string]activityParam{"actor": actor, "file": fileParam(mvID, "mv2.txt"), "oldfile": fileParam(mvID, "mv2.txt")})

	// COPY → exactly one file_created for the destination, even for a folder
	// (the inner Mkdir/Write verbs stay muted — no per-child fan-out).
	if _, _, err := env.dav.Copy(ctx, "alice", "/x", "alice", "/y", false, true); err != nil {
		t.Fatal(err)
	}
	yID := fileID("/y")
	wantRow(9, "file_created", "{actor} created {file}", "alice created y", "y", yID,
		map[string]activityParam{"actor": actor, "file": fileParam(yID, "y")})
	if _, err := env.dav.Stat(ctx, "alice", "/y/mv2.txt"); err != nil {
		t.Fatalf("folder copy lost its child: %v", err)
	}

	// Trash Restore → file_restored.
	if err := env.dav.Remove(ctx, "alice", "/y/mv2.txt"); err != nil {
		t.Fatal(err)
	}
	ents, err := env.dav.Trash.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 2 {
		t.Fatalf("trash list = %v %v", ents, err)
	}
	loc := ""
	for _, e := range ents {
		if strings.HasPrefix(e.Path, "/mv2.txt.") {
			loc = strings.TrimPrefix(e.Path, "/")
		}
	}
	if loc == "" {
		t.Fatalf("trash list missing mv2.txt: %v", ents)
	}
	if _, _, err := env.dav.Trash.Restore(ctx, "alice", loc, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	restoredID := fileID("/y/mv2.txt")
	wantRow(11, "file_restored", "{actor} restored {file}", "alice restored mv2.txt", "mv2.txt", restoredID,
		map[string]activityParam{"actor": actor, "file": fileParam(restoredID, "mv2.txt")})

	// The OCS layer: desc order, plaintext verbatim (no markers anywhere),
	// and X-Activity-Last-Given naming the oldest row of the page.
	rows := activityRowsFor(t, env.db, aliceID)
	data, body, lastGiven := listActivityOCS(t, store, env.users, nil,
		&auth.Principal{UID: "alice", Enabled: true})
	if len(data) != len(rows) {
		t.Fatalf("OCS data = %d rows, want %d", len(data), len(rows))
	}
	for i, item := range data {
		m, _ := item.(map[string]any)
		gotID, _ := m["activity_id"].(float64)
		want := rows[len(rows)-1-i].id // desc
		if int64(gotID) != want {
			t.Fatalf("data[%d] activity_id = %v, want %d (desc)", i, gotID, want)
		}
		_, _, params := ocsItem(t, item)
		assertNoSchemeMarker(t, params)
	}
	if lastGiven != strconv.FormatInt(rows[0].id, 10) {
		t.Errorf("X-Activity-Last-Given = %q, want %d (oldest row of the page)", lastGiven, rows[0].id)
	}
	subject, objectName, _ := ocsItem(t, data[0])
	if subject != "alice restored mv2.txt" || objectName != "mv2.txt" {
		t.Errorf("newest OCS item = %q %q, want the verbatim restore row", subject, objectName)
	}
	if strings.Contains(body, "ncgoNameScheme") {
		t.Error("scheme-0 OCS body carries the marker")
	}
}

// TestActivityPurgeSilent pins the trash-less delete carve-out: with no
// trashbin wired the Purge fallback emits nothing (upstream purge parity).
func TestActivityPurgeSilent(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	env.dav.Activity = activity.NewSQLStore(env.db)
	env.dav.Trash = nil

	env.write(t, "/gone.txt", "x")
	if err := env.dav.Remove(ctx, "alice", "/gone.txt"); err != nil {
		t.Fatal(err)
	}
	rows := activityRowsFor(t, env.db, env.ids["alice"])
	if len(rows) != 1 || rows[0].typ != "file_created" {
		t.Fatalf("rows = %+v, want exactly the create event (purge stays silent)", rows)
	}
}

// TestActivityFileLifecycleScheme1Master pins the §9 token shape for a
// scheme-1 (master-wrapped) owner: the stored subject and file param carry
// the ShareSubjectMeta token under the row's own key with the marker naming
// that key's UUID hex, and the OCS render decrypts in the viewer's ctx —
// plaintext subject/object_name, markers stripped, no token on the wire.
func TestActivityFileLifecycleScheme1Master(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")
	store := activity.NewSQLStore(env.db)
	env.dav.Activity = store
	aliceID := env.ids["alice"]

	env.mkdir(t, "/docs")
	env.write(t, "/docs/hello.txt", "hi")

	rows := activityRowsFor(t, env.db, aliceID)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (mkdir + write)", rows)
	}
	// Each stored row's file param is the ShareSubjectMeta token of the row's
	// own key (the §9 primitive, compared by a direct call — the stored
	// filecache Name is a DIFFERENT token under the parent key).
	storedTokens := make([]string, 0, 2)
	for i, p := range []string{"/docs", "/docs/hello.txt"} {
		raw := env.rawRow(t, p)
		token, keyHex, err := env.xlate.ShareSubjectMeta(ctx, aliceID, raw.Path)
		if err != nil {
			t.Fatal(err)
		}
		if keyHex != hex.EncodeToString(raw.KeyUUID) {
			t.Errorf("row %d marker hex = %q, want the row's own key %q", i, keyHex, hex.EncodeToString(raw.KeyUUID))
		}
		r := rows[i]
		wantSubject := "alice created " + token
		if r.subject != wantSubject {
			t.Errorf("row %d subject = %q, want %q (token embedded)", i, r.subject, wantSubject)
		}
		if r.objectName != token {
			t.Errorf("row %d object_name = %q, want the token", i, r.objectName)
		}
		pm := activityParams(t, r.params)
		if pm["file"] != (activityParam{Type: "file", ID: strconv.FormatInt(raw.ID, 10), Name: token}) {
			t.Errorf("row %d file param = %+v, want the token under id %d", i, pm["file"], raw.ID)
		}
		if pm["ncgoNameScheme"] != (activityParam{Type: "ncgo", ID: keyHex, Name: "1"}) {
			t.Errorf("row %d marker = %+v, want {ncgo %s 1}", i, pm["ncgoNameScheme"], keyHex)
		}
		if pm["file"].Name == raw.Name {
			t.Errorf("row %d file param equals the parent-key row token — wrong primitive", i)
		}
		storedTokens = append(storedTokens, token)
	}

	// The OCS render as the owner: plaintext subjects and object names,
	// markers gone, and neither the activity tokens nor any filecache token
	// reaches the wire (exact full-value absence, never substring matches).
	data, body, _ := listActivityOCS(t, store, env.users, env.xlate,
		&auth.Principal{UID: "alice", Enabled: true})
	if len(data) != 2 {
		t.Fatalf("OCS data = %v", data)
	}
	subject, objectName, params := ocsItem(t, data[0]) // desc: hello first
	if subject != "alice created hello.txt" || objectName != "hello.txt" {
		t.Errorf("rendered = %q %q, want plaintext hello.txt row", subject, objectName)
	}
	fileParam, _ := params["file"].(map[string]any)
	if fileParam["name"] != "hello.txt" || fileParam["type"] != "file" {
		t.Errorf("rendered file param = %v", fileParam)
	}
	assertNoSchemeMarker(t, params)
	subject, objectName, params = ocsItem(t, data[1])
	if subject != "alice created docs" || objectName != "docs" {
		t.Errorf("rendered = %q %q, want plaintext docs row", subject, objectName)
	}
	assertNoSchemeMarker(t, params)
	for _, token := range storedTokens {
		if strings.Contains(body, token) {
			t.Errorf("OCS body leaks the activity token %q", token)
		}
	}
	for _, token := range filecacheTokens(t, env.db, aliceID) {
		if strings.Contains(body, token) {
			t.Errorf("OCS body leaks the filecache token %q", token)
		}
	}
}

// TestActivityRenameScheme1DoubleMarker pins the rename concretion of §9:
// one file_renamed carries TWO tokenized names — "file" (new) sealed by the
// exact marker and "oldfile" (pre-move) sealed by "ncgoNameScheme:oldfile" —
// and the render decrypts both params and strips both markers. Same-directory
// and cross-directory moves share the shape (the marker names the row's own
// key, which a move never changes).
func TestActivityRenameScheme1DoubleMarker(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")
	store := activity.NewSQLStore(env.db)
	env.dav.Activity = store
	aliceID := env.ids["alice"]

	env.mkdir(t, "/a")
	env.mkdir(t, "/b")
	env.write(t, "/a/f.txt", "data")

	// Pre-move: the old name's §9 token under the row's own key.
	oldRaw := env.rawRow(t, "/a/f.txt")
	oldToken, oldHex, err := env.xlate.ShareSubjectMeta(ctx, aliceID, oldRaw.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Move(ctx, "alice", "/a/f.txt", "alice", "/a/g.txt", false); err != nil {
		t.Fatal(err)
	}
	newRaw := env.rawRow(t, "/a/g.txt")
	newToken, newHex, err := env.xlate.ShareSubjectMeta(ctx, aliceID, newRaw.Path)
	if err != nil {
		t.Fatal(err)
	}
	if newRaw.ID != oldRaw.ID || newHex != oldHex {
		t.Fatal("same-dir rename changed the row or its key")
	}

	rows := activityRowsFor(t, env.db, aliceID)
	r := rows[len(rows)-1]
	if r.typ != "file_renamed" {
		t.Fatalf("last row = %+v, want file_renamed", r)
	}
	if r.subject != "alice renamed "+oldToken+" to "+newToken {
		t.Errorf("stored subject = %q, want both tokens embedded", r.subject)
	}
	if r.objectID != newRaw.ID || r.objectName != newToken {
		t.Errorf("object = %d %q, want %d + the new token", r.objectID, r.objectName, newRaw.ID)
	}
	pm := activityParams(t, r.params)
	if pm["file"].Name != newToken || pm["oldfile"].Name != oldToken {
		t.Errorf("rename params = %+v, want new+old tokens", pm)
	}
	if pm["ncgoNameScheme"] != (activityParam{Type: "ncgo", ID: newHex, Name: "1"}) {
		t.Errorf("file marker = %+v", pm["ncgoNameScheme"])
	}
	if pm["ncgoNameScheme:oldfile"] != (activityParam{Type: "ncgo", ID: oldHex, Name: "1"}) {
		t.Errorf("oldfile marker = %+v", pm["ncgoNameScheme:oldfile"])
	}

	// Render: both params decrypted, both markers stripped.
	data, body, _ := listActivityOCS(t, store, env.users, env.xlate,
		&auth.Principal{UID: "alice", Enabled: true})
	subject, objectName, params := ocsItem(t, data[0])
	if subject != "alice renamed f.txt to g.txt" || objectName != "g.txt" {
		t.Errorf("rendered rename = %q %q", subject, objectName)
	}
	assertNoSchemeMarker(t, params)
	oldfile, _ := params["oldfile"].(map[string]any)
	if oldfile["name"] != "f.txt" {
		t.Errorf("rendered oldfile = %v", oldfile)
	}
	for _, token := range []string{oldToken, newToken} {
		if strings.Contains(body, token) {
			t.Errorf("render leaks the token %q", token)
		}
	}

	// Cross-directory move: same double-marker shape; the deterministic old
	// token of "g.txt" under the same row key reproduces as oldfile.
	if _, _, err := env.dav.Move(ctx, "alice", "/a/g.txt", "alice", "/b/g.txt", false); err != nil {
		t.Fatal(err)
	}
	rows = activityRowsFor(t, env.db, aliceID)
	r = rows[len(rows)-1]
	if r.typ != "file_renamed" {
		t.Fatalf("last row = %+v, want file_renamed", r)
	}
	pm = activityParams(t, r.params)
	if pm["oldfile"].Name != newToken || pm["file"].Name != newToken {
		t.Errorf("cross-dir params = %+v, want the deterministic g.txt token twice", pm)
	}
	if pm["ncgoNameScheme"].ID != newHex || pm["ncgoNameScheme:oldfile"].ID != newHex {
		t.Errorf("cross-dir markers = %+v, want the unchanged row key", pm)
	}
	data, body, _ = listActivityOCS(t, store, env.users, env.xlate,
		&auth.Principal{UID: "alice", Enabled: true})
	subject, _, params = ocsItem(t, data[0])
	if subject != "alice renamed g.txt to g.txt" {
		t.Errorf("rendered cross-dir rename = %q", subject)
	}
	assertNoSchemeMarker(t, params)
	if strings.Contains(body, newToken) {
		t.Error("cross-dir render leaks the token")
	}
}

// TestActivityDeleteScheme1PreCapture pins the delete pre-capture: the name
// material is derived while the row still exists, so the file_deleted row's
// token and marker hex equal the pre-delete ShareSubjectMeta values even
// though the filecache row is gone by assertion time.
func TestActivityDeleteScheme1PreCapture(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")
	store := activity.NewSQLStore(env.db)
	env.dav.Activity = store
	aliceID := env.ids["alice"]

	env.write(t, "/del.txt", "x")
	preRaw := env.rawRow(t, "/del.txt")
	preToken, preHex, err := env.xlate.ShareSubjectMeta(ctx, aliceID, preRaw.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.dav.Remove(ctx, "alice", "/del.txt"); err != nil {
		t.Fatal(err)
	}

	rows := activityRowsFor(t, env.db, aliceID)
	r := rows[len(rows)-1]
	if r.typ != "file_deleted" {
		t.Fatalf("last row = %+v, want file_deleted", r)
	}
	pm := activityParams(t, r.params)
	if pm["file"].Name != preToken || pm["ncgoNameScheme"].ID != preHex {
		t.Errorf("delete row = %+v, want the pre-capture token %q hex %q", pm, preToken, preHex)
	}
	if r.subject != "alice deleted "+preToken || r.objectName != preToken {
		t.Errorf("delete subject/object = %q %q, want the pre-capture token", r.subject, r.objectName)
	}

	data, body, _ := listActivityOCS(t, store, env.users, env.xlate,
		&auth.Principal{UID: "alice", Enabled: true})
	subject, objectName, params := ocsItem(t, data[0])
	if subject != "alice deleted del.txt" || objectName != "del.txt" {
		t.Errorf("rendered delete = %q %q", subject, objectName)
	}
	assertNoSchemeMarker(t, params)
	if strings.Contains(body, preToken) {
		t.Error("delete render leaks the token")
	}
}

// TestActivityFileLifecycleEnrolledPW pins the enrolled-owner boundary
// (ADR-0100/0101 + §9): the owner's unlocked-session view decrypts, while an
// app-password (no UnlockedKey) view degrades every name to the "encrypted
// file" placeholder — per item, list intact, token never on the wire.
func TestActivityFileLifecycleEnrolledPW(t *testing.T) {
	env := newPWEnv(t, "alice")
	e2e := upgradeNameCrypt(t, &env.keyShareEnv, env.res)
	e2e.encryptUser(t, "alice")
	aliceKey := env.login(t, "alice", "alice-pw")
	actx := pctx("alice", aliceKey)
	store := activity.NewSQLStore(env.db)
	env.dav.Activity = store

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/plans.txt", strings.NewReader("p"), nil); err != nil {
		t.Fatal(err)
	}

	// At rest: both rows tokenized (exact comparisons against the direct
	// §9 primitive — never a plaintext window).
	rows := activityRowsFor(t, env.db, env.ids["alice"])
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	storedTokens := make([]string, 0, 2)
	for i, p := range []string{"/secret", "/secret/plans.txt"} {
		raw := e2e.rawRowAs(t, actx, p)
		token, _, err := e2e.xlate.ShareSubjectMeta(actx, env.ids["alice"], raw.Path)
		if err != nil {
			t.Fatal(err)
		}
		if rows[i].subject != "alice created "+token {
			t.Errorf("row %d subject = %q, want the token form", i, rows[i].subject)
		}
		storedTokens = append(storedTokens, token)
	}

	// Unlocked session: the view decrypts.
	data, _, _ := listActivityOCS(t, store, env.users, e2e.xlate,
		&auth.Principal{UID: "alice", Enabled: true, AuthMethod: auth.AuthMethodSession, UnlockedKey: aliceKey})
	if len(data) != 2 {
		t.Fatalf("unlocked data = %v", data)
	}
	subject, objectName, _ := ocsItem(t, data[0])
	if subject != "alice created plans.txt" || objectName != "plans.txt" {
		t.Errorf("unlocked render = %q %q", subject, objectName)
	}

	// App password (no UnlockedKey): every name degrades to the placeholder,
	// markers stripped, list 200, and the tokens never appear (full-value
	// absence — the token string itself is the assertion unit).
	data, body, _ := listActivityOCS(t, store, env.users, e2e.xlate,
		&auth.Principal{UID: "alice", Enabled: true, AuthMethod: auth.AuthMethodAppPassword})
	if len(data) != 2 {
		t.Fatalf("keyless data = %v", data)
	}
	for _, item := range data {
		subject, objectName, params := ocsItem(t, item)
		if subject != "alice created "+activity.EncryptedNamePlaceholder {
			t.Errorf("keyless subject = %q, want the placeholder form", subject)
		}
		if objectName != activity.EncryptedNamePlaceholder {
			t.Errorf("keyless object_name = %q, want the placeholder", objectName)
		}
		fileParam, _ := params["file"].(map[string]any)
		if fileParam["name"] != activity.EncryptedNamePlaceholder {
			t.Errorf("keyless file param = %v", fileParam)
		}
		assertNoSchemeMarker(t, params)
	}
	for _, token := range storedTokens {
		if strings.Contains(body, token) {
			t.Errorf("keyless body leaks the token %q", token)
		}
	}
}

// TestActivityCipherMountWriteFlowsToOwner pins the mounted-share stream
// routing: a sharee's write inside a (master-wrapped) ciphertext mount lands
// in the MOUNT OWNER's stream with the sharee as actor, and the owner's view
// decrypts it.
func TestActivityCipherMountWriteFlowsToOwner(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice")
	store := activity.NewSQLStore(env.db)
	env.dav.Activity = store
	aliceID := env.ids["alice"]

	env.mkdir(t, "/shared")
	if _, err := env.svc.Create(ctx, "alice", "/shared", files.ShareTypeUser,
		webdav.PermRead|webdav.PermUpdate|webdav.PermCreate, "bob", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	env.dav.Incoming = files.MultiIncoming{env.svc}

	if _, _, err := env.dav.Write(ctx, "bob", "/shared/bob.txt", strings.NewReader("b"), nil); err != nil {
		t.Fatal(err)
	}

	// Alice's stream gains the event with bob as the actor; bob's own stream
	// stays empty (mount writes flow to the owner).
	rows := activityRowsFor(t, env.db, aliceID)
	r := rows[len(rows)-1]
	if r.typ != "file_created" || r.actor != "bob" {
		t.Fatalf("owner stream row = %+v, want file_created actor bob", r)
	}
	pm := activityParams(t, r.params)
	if pm["actor"].ID != "bob" || pm["actor"].Type != "user" {
		t.Errorf("actor param = %+v, want user bob", pm["actor"])
	}
	raw := env.rawRow(t, "/shared/bob.txt")
	token, keyHex, err := env.xlate.ShareSubjectMeta(ctx, aliceID, raw.Path)
	if err != nil {
		t.Fatal(err)
	}
	if pm["file"].Name != token || pm["ncgoNameScheme"].ID != keyHex {
		t.Errorf("mount write row = %+v, want the §9 token shape", pm)
	}
	if bobRows := activityRowsFor(t, env.db, env.ids["bob"]); len(bobRows) != 0 {
		t.Errorf("sharee stream = %+v, want empty", bobRows)
	}

	data, body, _ := listActivityOCS(t, store, env.users, env.xlate,
		&auth.Principal{UID: "alice", Enabled: true})
	subject, objectName, params := ocsItem(t, data[0])
	if subject != "bob created bob.txt" || objectName != "bob.txt" {
		t.Errorf("owner render = %q %q", subject, objectName)
	}
	assertNoSchemeMarker(t, params)
	if strings.Contains(body, token) {
		t.Error("owner render leaks the token")
	}
}

// TestActivityCipherMountEnrolledSkipsCapture pins the documented residual:
// an ENROLLED mount owner's tree cannot be name-resolved above the share
// root in the sharee's ctx, so the sharee's in-mount write skips its
// activity event (the §9 skip rule — never a plaintext fallback) while the
// write itself succeeds.
func TestActivityCipherMountEnrolledSkipsCapture(t *testing.T) {
	rig := newCipherMountRig(t)
	env := rig.env
	env.dav.Activity = activity.NewSQLStore(env.db)
	actx := rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	rig.shareSecret(t, webdav.PermRead|webdav.PermUpdate|webdav.PermCreate)

	before := activityRowsFor(t, env.db, env.ids["alice"])
	bctx := pctx("bob", rig.bobKey)
	if _, _, err := env.dav.Write(bctx, "bob", "/secret/bob.txt", strings.NewReader("b"), nil); err != nil {
		t.Fatal(err)
	}
	after := activityRowsFor(t, env.db, env.ids["alice"])
	if len(after) != len(before) {
		t.Errorf("enrolled mount write emitted %d rows, want the capture skipped (write succeeds)", len(after)-len(before))
	}
	// The write itself landed in the owner's tree.
	if _, err := env.dav.Stat(actx, "alice", "/secret/bob.txt"); err != nil {
		t.Fatalf("sharee write lost: %v", err)
	}
}

// TestActivitySubjectTamperDegrades pins the render degradation (§9): a
// tampered stored token fails authentication and the item degrades to the
// "encrypted file" placeholder — the list stays 200, other items render
// normally, and neither the tampered nor the intact token reaches the wire.
func TestActivitySubjectTamperDegrades(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")
	store := activity.NewSQLStore(env.db)
	env.dav.Activity = store
	aliceID := env.ids["alice"]

	env.write(t, "/good.txt", "g")
	env.write(t, "/bad.txt", "b")

	rows := activityRowsFor(t, env.db, aliceID)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	good := activityParams(t, rows[0].params)["file"].Name
	// Tamper the SECOND row's stored token (params + stored subject).
	pm := activityParams(t, rows[1].params)
	tampered := pm["file"]
	tampered.Name = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	pm["file"] = tampered
	buf, err := json.Marshal(pm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx,
		`UPDATE activities SET subject = ?, subject_rich_parameters = ? WHERE id = ?`,
		"alice created "+tampered.Name, string(buf), rows[1].id); err != nil {
		t.Fatal(err)
	}

	data, body, _ := listActivityOCS(t, store, env.users, env.xlate,
		&auth.Principal{UID: "alice", Enabled: true})
	if len(data) != 2 {
		t.Fatalf("data = %v, want the list intact", data)
	}
	subject, objectName, _ := ocsItem(t, data[0]) // desc: bad.txt first
	if subject != "alice created "+activity.EncryptedNamePlaceholder || objectName != activity.EncryptedNamePlaceholder {
		t.Errorf("tampered render = %q %q, want the placeholder", subject, objectName)
	}
	subject, objectName, _ = ocsItem(t, data[1])
	if subject != "alice created good.txt" || objectName != "good.txt" {
		t.Errorf("intact render = %q %q, want plaintext", subject, objectName)
	}
	for _, token := range []string{good, tampered.Name} {
		if strings.Contains(body, token) {
			t.Errorf("body leaks the token %q", token)
		}
	}
}
