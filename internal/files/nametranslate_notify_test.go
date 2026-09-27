package files_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/notifications"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// notifRichParam mirrors the producer's rich-object parameter shape for
// assertions.
type notifRichParam struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// notifRows reads the raw notifications table for the env's own users (test
// envs within one test share the in-memory DB) — the phase-3b at-rest view.
func (e *nameE2EEnv) notifRows(t *testing.T) []struct {
	userID  int64
	subject string
	rich    string
	params  string
} {
	t.Helper()
	ids := make([]int64, 0, len(e.ids))
	for _, id := range e.ids {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	marks := strings.Repeat("?,", len(ids))
	marks = marks[:len(marks)-1]
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := e.db.Query(context.Background(),
		`SELECT user_id, subject, subject_rich, subject_rich_parameters FROM notifications WHERE user_id IN (`+marks+`) ORDER BY user_id`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct {
		userID  int64
		subject string
		rich    string
		params  string
	}
	for rows.Next() {
		var r struct {
			userID  int64
			subject string
			rich    string
			params  string
		}
		if err := rows.Scan(&r.userID, &r.subject, &r.rich, &r.params); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// listNotifs serves the OCS notifications list as uid through a handler
// carrying the phase-3b subject decryptor, returning the decoded data array
// and the raw body.
func (e *nameE2EEnv) listNotifs(t *testing.T, p *auth.Principal, notifs *notifications.SQLStore) ([]any, string) {
	t.Helper()
	h := notifications.Handler{Store: notifs, Users: e.users, Version: ocs.V2, Subjects: e.xlate}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/ocs/v2.php/apps/notifications/api/v2/notifications?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), p))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("notifications list as %s = %d: %s", p.UID, rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data, ok := env["ocs"].(map[string]any)["data"].([]any)
	if !ok {
		t.Fatalf("data shape = %s", rr.Body.String())
	}
	return data, rr.Body.String()
}

// TestNameCryptShareNotificationTokenSubject pins the phase-3b producer shape
// (ADR-0104 §9): a scheme-1 owner's share bell stores the mount-name TOKEN in
// the subject and rich param — byte-identical to the share row's
// mount_name_enc (deterministic) — plus the "ncgoNameScheme" marker carrying
// the target key's UUID hex, with no plaintext path material anywhere. The
// OCS render decrypts in the viewer's ctx and rebuilds the subject from the
// rich template (basename display — a sharee cannot resolve ancestors).
// Scheme-0 rows stay bit-identical and pass the render verbatim.
func TestNameCryptShareNotificationTokenSubject(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice")
	notifs := notifications.NewSQLStore(env.db)
	env.svc.Notifs = notifs

	env.mkdir(t, "/docs")
	env.write(t, "/docs/report.xlsx", "sheet")
	sh := env.share(t, "/docs/report.xlsx", files.ShareTypeUser, "bob")

	rows := env.notifRows(t)
	if len(rows) != 1 || rows[0].userID != env.ids["bob"] {
		t.Fatalf("notification rows = %+v", rows)
	}
	row := rows[0]
	_, mountEnc, _ := env.shareRow(t, sh.ID)
	if mountEnc == "" {
		t.Fatal("share row carries no mount_name_enc")
	}
	wantSubject := "You received " + mountEnc + " as a share by alice"
	if row.subject != wantSubject {
		t.Errorf("stored subject = %q, want %q (token == mount_name_enc)", row.subject, wantSubject)
	}
	for _, leak := range []string{"docs", "report", "xlsx"} {
		if strings.Contains(row.subject, leak) || strings.Contains(row.params, leak) {
			t.Errorf("notification row leaks %q: subject=%q params=%q", leak, row.subject, row.params)
		}
	}
	if row.rich != "You received {share} as a share by {user}" {
		t.Errorf("subject_rich = %q", row.rich)
	}
	var pm map[string]notifRichParam
	if err := json.Unmarshal([]byte(row.params), &pm); err != nil {
		t.Fatal(err)
	}
	objectID := "ocinternal:" + strconv.FormatInt(sh.ID, 10)
	if pm["share"] != (notifRichParam{Type: "highlight", ID: objectID, Name: mountEnc}) {
		t.Errorf("share param = %+v, want the token under id %s", pm["share"], objectID)
	}
	targetUUID := hex.EncodeToString(env.rawRow(t, "/docs/report.xlsx").KeyUUID)
	if pm["ncgoNameScheme"] != (notifRichParam{Type: "ncgo", ID: targetUUID, Name: "1"}) {
		t.Errorf("marker param = %+v, want {ncgo %s 1}", pm["ncgoNameScheme"], targetUUID)
	}
	if pm["user"] != (notifRichParam{Type: "user", ID: "alice", Name: "alice"}) {
		t.Errorf("user param = %+v", pm["user"])
	}

	// The OCS render as the sharee: subject rebuilt from the template with
	// the decrypted BASENAME, the marker stripped, the token nowhere.
	data, body := env.listNotifs(t, &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodSession}, notifs)
	if len(data) != 1 {
		t.Fatalf("bob list = %v", data)
	}
	n, _ := data[0].(map[string]any)
	if n["subject"] != "You received report.xlsx as a share by alice" {
		t.Errorf("rendered subject = %v", n["subject"])
	}
	params, _ := n["subjectRichParameters"].(map[string]any)
	if _, marked := params["ncgoNameScheme"]; marked {
		t.Errorf("rendered params still carry the marker: %v", params)
	}
	shareParam, _ := params["share"].(map[string]any)
	if shareParam["name"] != "report.xlsx" || shareParam["type"] != "highlight" || shareParam["id"] != objectID {
		t.Errorf("rendered share param = %v", shareParam)
	}
	if strings.Contains(body, mountEnc) {
		t.Error("rendered list leaks the name token")
	}

	// The single-notification GET applies the same render (no token there
	// either).
	h := notifications.Handler{Store: notifs, Users: env.users, Version: ocs.V2, Subjects: env.xlate}
	var notifID int64
	if err := env.db.QueryRow(ctx, `SELECT id FROM notifications WHERE user_id = ?`, env.ids["bob"]).Scan(&notifID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/ocs/v2.php/apps/notifications/api/v2/notifications/"+strconv.FormatInt(notifID, 10)+"?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodSession}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "report.xlsx") || strings.Contains(rr.Body.String(), mountEnc) {
		t.Errorf("single GET = %d %s", rr.Code, rr.Body.String())
	}

	// Scheme 0 (carol → dave): today's exact plaintext row, verbatim render
	// through the same decryptor-wired handler (unmarked passthrough).
	env0 := newNameE2EEnv(t, "carol", "dave")
	notifs0 := notifications.NewSQLStore(env0.db)
	env0.svc.Notifs = notifs0
	if _, err := env0.dav.Mkdir(ctx, "carol", "/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := env0.svc.Create(ctx, "carol", "/plain", files.ShareTypeUser, 0, "dave", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rows0 := env0.notifRows(t)
	if len(rows0) != 1 {
		t.Fatalf("scheme-0 rows = %+v", rows0)
	}
	if rows0[0].subject != "You received /plain as a share by carol" {
		t.Errorf("scheme-0 subject = %q, want the verbatim plaintext form", rows0[0].subject)
	}
	if strings.Contains(rows0[0].params, "ncgoNameScheme") {
		t.Errorf("scheme-0 params carry the marker: %q", rows0[0].params)
	}
	data0, _ := env0.listNotifs(t, &auth.Principal{UID: "dave", Enabled: true}, notifs0)
	if len(data0) != 1 {
		t.Fatalf("dave list = %v", data0)
	}
	n0, _ := data0[0].(map[string]any)
	if n0["subject"] != "You received /plain as a share by carol" {
		t.Errorf("scheme-0 rendered subject = %v, want verbatim", n0["subject"])
	}
	params0, _ := n0["subjectRichParameters"].(map[string]any)
	share0, _ := params0["share"].(map[string]any)
	if share0["name"] != "/plain" {
		t.Errorf("scheme-0 rendered share param = %v, want the full plaintext path", share0)
	}
}

// TestNameCryptShareNotificationGroupTokenized pins the group-share variant:
// every member's row carries the tokenized shape (and the owner's row is
// skipped, as today).
func TestNameCryptShareNotificationGroupTokenized(t *testing.T) {
	env := newNameE2EEnv(t, "alice", "bob", "carol")
	env.encryptUser(t, "alice")
	notifs := notifications.NewSQLStore(env.db)
	env.svc.Notifs = notifs
	if err := env.users.CreateGroup(context.Background(), &users.Group{GID: "g1", DisplayName: "Group 1"}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"bob", "carol"} {
		if err := env.users.AddGroupMember(context.Background(), "g1", uid); err != nil {
			t.Fatal(err)
		}
	}

	env.mkdir(t, "/docs")
	sh := env.share(t, "/docs", files.ShareTypeGroup, "g1")
	_, mountEnc, _ := env.shareRow(t, sh.ID)
	targetUUID := hex.EncodeToString(env.rawRow(t, "/docs").KeyUUID)

	rows := env.notifRows(t)
	if len(rows) != 2 {
		t.Fatalf("group notification rows = %+v, want bob+carol", rows)
	}
	wantSubject := "You received " + mountEnc + " to group g1 as a share by alice"
	for _, r := range rows {
		if r.userID != env.ids["bob"] && r.userID != env.ids["carol"] {
			t.Errorf("unexpected recipient %d", r.userID)
		}
		if r.subject != wantSubject {
			t.Errorf("row %d subject = %q, want %q", r.userID, r.subject, wantSubject)
		}
		var pm map[string]notifRichParam
		if err := json.Unmarshal([]byte(r.params), &pm); err != nil {
			t.Fatal(err)
		}
		if pm["ncgoNameScheme"] != (notifRichParam{Type: "ncgo", ID: targetUUID, Name: "1"}) {
			t.Errorf("row %d marker = %+v", r.userID, pm["ncgoNameScheme"])
		}
		if pm["share"].Name != mountEnc || pm["group"].Name != "Group 1" {
			t.Errorf("row %d params = %+v", r.userID, pm)
		}
		if strings.Contains(r.subject, "docs") || strings.Contains(r.params, "docs") {
			t.Errorf("row %d leaks the plaintext name", r.userID)
		}
	}

	// A member's OCS render: basename + group substituted, marker gone. The
	// rebuilt subject follows the rich template, so {group} renders the
	// display name (the stored plaintext form used the gid — clients that
	// render rich subjects already show this form).
	data, body := env.listNotifs(t, &auth.Principal{UID: "carol", Enabled: true}, notifs)
	if len(data) != 1 {
		t.Fatalf("carol list = %v", data)
	}
	n, _ := data[0].(map[string]any)
	if n["subject"] != "You received docs to group Group 1 as a share by alice" {
		t.Errorf("carol rendered subject = %v", n["subject"])
	}
	if strings.Contains(body, mountEnc) || strings.Contains(body, "ncgoNameScheme") {
		t.Error("carol render leaks the token or the marker")
	}
}

// TestNameCryptShareNotificationViewerLocked pins the render degradation
// (ADR-0104 §9): an enrolled sharee resolves the token through their own wrap
// with an unlocked session; keyless, the item degrades to the "encrypted
// file" placeholder — per item, never failing the list, never emitting the
// token. A tampered token degrades the same way.
func TestNameCryptShareNotificationViewerLocked(t *testing.T) {
	env := newPWEnv(t, "alice", "bob")
	e2e := upgradeNameCrypt(t, &env.keyShareEnv, env.res)
	e2e.encryptUser(t, "alice")
	e2e.encryptUser(t, "bob")
	aliceKey := env.login(t, "alice", "alice-pw")
	bobKey := env.login(t, "bob", "bob-pw")
	actx := pctx("alice", aliceKey)
	notifs := notifications.NewSQLStore(env.db)
	env.svc.Notifs = notifs

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	sh, err := env.svc.Create(actx, "alice", "/secret", files.ShareTypeUser, 0, "bob", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, mountEnc, _ := e2e.shareRow(t, sh.ID)
	if mountEnc == "" {
		t.Fatal("share row carries no mount_name_enc")
	}

	// Unlocked sharee: the token opens through bob's own wrap row.
	data, _ := e2e.listNotifs(t, &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodSession, UnlockedKey: bobKey}, notifs)
	if len(data) != 1 {
		t.Fatalf("bob unlocked list = %v", data)
	}
	n, _ := data[0].(map[string]any)
	if n["subject"] != "You received secret as a share by alice" {
		t.Errorf("unlocked rendered subject = %v", n["subject"])
	}

	// Keyless sharee: placeholder, marker stripped, token nowhere, list fine.
	data, body := e2e.listNotifs(t, &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodAppPassword}, notifs)
	if len(data) != 1 {
		t.Fatalf("bob keyless list = %v", data)
	}
	n, _ = data[0].(map[string]any)
	if n["subject"] != "You received "+notifications.EncryptedNamePlaceholder+" as a share by alice" {
		t.Errorf("keyless rendered subject = %v", n["subject"])
	}
	params, _ := n["subjectRichParameters"].(map[string]any)
	shareParam, _ := params["share"].(map[string]any)
	if shareParam["name"] != notifications.EncryptedNamePlaceholder {
		t.Errorf("keyless share param = %v", shareParam)
	}
	if _, marked := params["ncgoNameScheme"]; marked {
		t.Errorf("keyless params still carry the marker: %v", params)
	}
	if strings.Contains(body, mountEnc) {
		t.Error("keyless render leaks the token")
	}

	// A tampered token fails authentication and degrades the same way (the
	// list survives; the placeholder stands in).
	var rawParams string
	var notifID int64
	if err := env.db.QueryRow(context.Background(),
		`SELECT id, subject_rich_parameters FROM notifications WHERE user_id = ?`, env.ids["bob"]).Scan(&notifID, &rawParams); err != nil {
		t.Fatal(err)
	}
	var pm map[string]notifRichParam
	if err := json.Unmarshal([]byte(rawParams), &pm); err != nil {
		t.Fatal(err)
	}
	tampered := pm["share"]
	tampered.Name = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	pm["share"] = tampered
	buf, err := json.Marshal(pm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(context.Background(),
		`UPDATE notifications SET subject = ?, subject_rich_parameters = ? WHERE id = ?`,
		"You received "+tampered.Name+" as a share by alice", string(buf), notifID); err != nil {
		t.Fatal(err)
	}
	data, body = e2e.listNotifs(t, &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodSession, UnlockedKey: bobKey}, notifs)
	if len(data) != 1 {
		t.Fatalf("bob tampered list = %v", data)
	}
	n, _ = data[0].(map[string]any)
	if n["subject"] != "You received "+notifications.EncryptedNamePlaceholder+" as a share by alice" {
		t.Errorf("tampered rendered subject = %v", n["subject"])
	}
	if strings.Contains(body, tampered.Name) {
		t.Error("tampered render leaks the token")
	}
}
