package mail

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// mailEnv wires the handler behind the REAL auth middleware (webdav.Auth,
// exactly the routes.go mount) over a real sqlite store, so the tests
// exercise session-auth rejection and per-user scoping end to end.
type mailEnv struct {
	db     database.DB
	store  *SQLStore
	svc    *Service
	chain  http.Handler
	secret string
}

func newMailEnv(t *testing.T) *mailEnv {
	t.Helper()
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	hasher := auth.NewArgon2id(auth.Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1})
	for _, uid := range []string{"alice", "bob"} {
		hash, err := hasher.Hash(uid + "-pw")
		if err != nil {
			t.Fatal(err)
		}
		if err := us.Create(ctx, &users.User{UID: uid, DisplayName: uid, PasswordHash: hash, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	store := NewSQLStore(db)
	svc := &Service{Store: store, Secret: "test-instance-secret"}
	authCfg := auth.MiddlewareConfig{Verifier: users.NewPasswordVerifier(us, hasher)}
	return &mailEnv{
		db:     db,
		store:  store,
		svc:    svc,
		chain:  webdav.Auth(authCfg)(&Handler{Svc: svc}),
		secret: "test-instance-secret",
	}
}

// serve runs one request, authenticating as uid when non-empty, and asserts
// the response never carries either password half (ADR-0108 §2: passwords
// NEVER appear in responses — checked on every response of every test).
func (e *mailEnv) serve(t *testing.T, uid, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if uid != "" {
		req.SetBasicAuth(uid, uid+"-pw")
	}
	rr := httptest.NewRecorder()
	e.chain.ServeHTTP(rr, req)
	for _, pw := range []string{"imap-secret-pw", "smtp-secret-pw", "imap-secret-pw-2"} {
		if strings.Contains(rr.Body.String(), pw) {
			t.Fatalf("%s %s: response leaks password: %s", method, path, rr.Body.String())
		}
	}
	return rr
}

const createBodyAlice = `{"name":"Work","emailAddress":"alice@example.com","imapHost":"imap.example.com","imapPort":993,"imapSslMode":"ssl","imapUser":"alice@example.com","imapPassword":"imap-secret-pw","smtpHost":"smtp.example.com","smtpPort":465,"smtpSslMode":"ssl","smtpUser":"alice@example.com","smtpPassword":"smtp-secret-pw"}`

func createAlice(t *testing.T, e *mailEnv) int64 {
	t.Helper()
	rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix, createBodyAlice)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp accountResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID <= 0 {
		t.Fatalf("create: id = %d", resp.ID)
	}
	return resp.ID
}

func TestHandlerAuthGate(t *testing.T) {
	e := newMailEnv(t)
	if rr := e.serve(t, "", http.MethodGet, AccountsPrefix, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d", rr.Code)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, AccountsPrefix, nil)
	req.SetBasicAuth("alice", "wrong-pw")
	rr := httptest.NewRecorder()
	e.chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("bad password: status = %d", rr.Code)
	}
}

func TestHandlerCreateListGetUpdateDelete(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)

	// List: exactly one account, official field names, no password fields.
	rr := e.serve(t, "alice", http.MethodGet, AccountsPrefix, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: status = %d", rr.Code)
	}
	var list []accountResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id || list[0].EmailAddress != "alice@example.com" ||
		list[0].IMAPHost != "imap.example.com" || list[0].IMAPPort != 993 || list[0].IMAPSSLMode != "ssl" ||
		list[0].SMTPHost != "smtp.example.com" || list[0].SMTPPort != 465 || list[0].Name != "Work" {
		t.Errorf("list = %+v", list)
	}
	if strings.Contains(rr.Body.String(), "assword") {
		t.Errorf("list body carries a password field: %s", rr.Body.String())
	}

	// Get one: adds the forward-compat mailboxes placeholder.
	one := fmt.Sprintf("%s/%d", AccountsPrefix, id)
	rr = e.serve(t, "alice", http.MethodGet, one, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get: status = %d", rr.Code)
	}
	var detail map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	mb, ok := detail["mailboxes"].([]any)
	if !ok || len(mb) != 0 {
		t.Errorf("mailboxes = %v", detail["mailboxes"])
	}

	// Partial update: name + imap password only; smtp half must survive.
	rr = e.serve(t, "alice", http.MethodPut, one, `{"name":"Renamed","imapPassword":"imap-secret-pw-2"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var upd accountResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &upd); err != nil {
		t.Fatal(err)
	}
	if upd.Name != "Renamed" || upd.IMAPHost != "imap.example.com" || upd.IMAPPort != 993 {
		t.Errorf("updated = %+v", upd)
	}
	row, err := e.store.GetByID(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	imap, smtp, err := e.svc.passwords("alice", row.IMAPHost, row.IMAPUser, row.PasswordSealed)
	if err != nil {
		t.Fatal(err)
	}
	if imap != "imap-secret-pw-2" || smtp != "smtp-secret-pw" {
		t.Errorf("passwords after update = %q %q", imap, smtp)
	}

	// Delete: 200 {} then every access is 404.
	rr = e.serve(t, "alice", http.MethodDelete, one, "")
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "{}" {
		t.Fatalf("delete: status = %d body = %s", rr.Code, rr.Body.String())
	}
	if rr := e.serve(t, "alice", http.MethodGet, one, ""); rr.Code != http.StatusNotFound {
		t.Errorf("get after delete: status = %d", rr.Code)
	}
	if rr := e.serve(t, "alice", http.MethodGet, AccountsPrefix, ""); rr.Code != http.StatusOK ||
		strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Errorf("list after delete: status = %d body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerCrossUser404(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)
	one := fmt.Sprintf("%s/%d", AccountsPrefix, id)

	for _, tc := range []struct {
		method, body string
	}{
		{http.MethodGet, ""},
		{http.MethodPut, `{"name":"Evil"}`},
		{http.MethodDelete, ""},
	} {
		rr := e.serve(t, "bob", tc.method, one, tc.body)
		if rr.Code != http.StatusNotFound {
			t.Errorf("bob %s: status = %d, want 404", tc.method, rr.Code)
		}
	}
	// Bob's own list stays empty; Alice's row is untouched.
	if rr := e.serve(t, "bob", http.MethodGet, AccountsPrefix, ""); strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Errorf("bob list = %s", rr.Body.String())
	}
	if rr := e.serve(t, "alice", http.MethodGet, one, ""); rr.Code != http.StatusOK {
		t.Errorf("alice get after bob's attempts: status = %d", rr.Code)
	}
}

func TestHandlerValidation(t *testing.T) {
	e := newMailEnv(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `{`, "invalid JSON body"},
		{"missing email", `{"imapHost":"h","imapPort":993,"imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`, "emailAddress is required"},
		{"bad email", `{"emailAddress":"not-an-email","imapHost":"h","imapPort":993,"imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`, "emailAddress is invalid"},
		{"missing imap host", `{"emailAddress":"a@b.c","imapPort":993,"imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`, "imapHost is required"},
		{"missing imap user", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":993,"imapPassword":"p","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`, "imapUser is required"},
		{"missing imap password", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":993,"imapUser":"u","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`, "imapPassword is required"},
		{"imap port zero", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":0,"imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`, "imapPort must be between 1 and 65535"},
		{"imap port high", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":70000,"imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`, "imapPort must be between 1 and 65535"},
		{"bad imap ssl mode", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":993,"imapSslMode":"tls","imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`, "imapSslMode must be one of ssl, starttls, none"},
		{"missing smtp host", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":993,"imapUser":"u","imapPassword":"p","smtpPort":25,"smtpUser":"u"}`, "smtpHost is required"},
		{"missing smtp user", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":993,"imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":25}`, "smtpUser is required"},
		{"smtp port high", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":993,"imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":65536,"smtpUser":"u"}`, "smtpPort must be between 1 and 65535"},
		{"bad smtp ssl mode", `{"emailAddress":"a@b.c","imapHost":"h","imapPort":993,"imapUser":"u","imapPassword":"p","smtpHost":"h","smtpPort":25,"smtpSslMode":"auto","smtpUser":"u"}`, "smtpSslMode must be one of ssl, starttls, none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), `"error":"`+tc.want+`"`) {
				t.Errorf("body = %s, want error %q", rr.Body.String(), tc.want)
			}
		})
	}

	// Defaults: absent ssl modes seal as 'ssl'; absent smtp password reuses
	// the imap one; empty name is allowed.
	rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix,
		`{"emailAddress":"a@b.c","imapHost":"h","imapPort":143,"imapUser":"u","imapPassword":"imap-secret-pw","smtpHost":"h","smtpPort":25,"smtpUser":"u"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("defaults create: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp accountResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.IMAPSSLMode != "ssl" || resp.SMTPSSLMode != "ssl" {
		t.Errorf("ssl defaults = %q %q", resp.IMAPSSLMode, resp.SMTPSSLMode)
	}
	row, err := e.store.GetByID(t.Context(), "alice", resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	imap, smtp, err := e.svc.passwords("alice", row.IMAPHost, row.IMAPUser, row.PasswordSealed)
	if err != nil {
		t.Fatal(err)
	}
	if imap != "imap-secret-pw" || smtp != "imap-secret-pw" {
		t.Errorf("smtp password default = %q %q", imap, smtp)
	}

	// PUT validation failures (on the just-created id).
	one := fmt.Sprintf("%s/%d", AccountsPrefix, resp.ID)
	putCases := []struct {
		name string
		body string
		want string
	}{
		{"put bad email", `{"emailAddress":"nope"}`, "emailAddress is invalid"},
		{"put empty host", `{"imapHost":""}`, "imapHost is required"},
		{"put empty password", `{"imapPassword":""}`, "imapPassword is required"},
		{"put port low", `{"smtpPort":-1}`, "smtpPort must be between 1 and 65535"},
		{"put bad ssl mode", `{"imapSslMode":""}`, "imapSslMode must be one of ssl, starttls, none"},
		{"put not json", `[`, "invalid JSON body"},
	}
	for _, tc := range putCases {
		t.Run(tc.name, func(t *testing.T) {
			rr := e.serve(t, "alice", http.MethodPut, one, tc.body)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), `"error":"`+tc.want+`"`) {
				t.Errorf("status = %d body = %s, want 400 %q", rr.Code, rr.Body.String(), tc.want)
			}
		})
	}
}

func TestHandlerSealedAtRest(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)

	// Read the row directly: the at-rest blob must differ from both password
	// halves and open back to exactly them under the instance secret.
	var sealed []byte
	if err := e.db.QueryRow(t.Context(), `SELECT password_sealed FROM mail_accounts WHERE id = ?`, id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("imap-secret-pw")) || bytes.Contains(sealed, []byte("smtp-secret-pw")) {
		t.Fatal("password_sealed contains plaintext")
	}
	packed, err := OpenCredential(e.secret, "alice", "imap.example.com", "alice@example.com", sealed)
	if err != nil {
		t.Fatal(err)
	}
	imap, smtp, err := splitPasswords(packed)
	if err != nil {
		t.Fatal(err)
	}
	if imap != "imap-secret-pw" || smtp != "smtp-secret-pw" {
		t.Errorf("opened = %q %q", imap, smtp)
	}
	// Bound to the identity triple: bob's uid or another host does not open.
	if _, err := OpenCredential(e.secret, "bob", "imap.example.com", "alice@example.com", sealed); !errors.Is(err, ErrCredential) {
		t.Errorf("open as bob: err = %v", err)
	}
	if _, err := OpenCredential(e.secret, "alice", "imap.other.com", "alice@example.com", sealed); !errors.Is(err, ErrCredential) {
		t.Errorf("open with other host: err = %v", err)
	}
}

func TestHandlerHostChangeReseals(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)
	one := fmt.Sprintf("%s/%d", AccountsPrefix, id)

	// Moving the imap host re-binds the blob to the new AD without any
	// password field in the patch; the old AD must stop opening it.
	rr := e.serve(t, "alice", http.MethodPut, one, `{"imapHost":"imap.new.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: status = %d body = %s", rr.Code, rr.Body.String())
	}
	row, err := e.store.GetByID(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	imap, smtp, err := e.svc.passwords("alice", "imap.new.com", "alice@example.com", row.PasswordSealed)
	if err != nil {
		t.Fatal(err)
	}
	if imap != "imap-secret-pw" || smtp != "smtp-secret-pw" {
		t.Errorf("passwords after host change = %q %q", imap, smtp)
	}
	if _, err := OpenCredential(e.secret, "alice", "imap.example.com", "alice@example.com", row.PasswordSealed); !errors.Is(err, ErrCredential) {
		t.Errorf("old AD still opens the resealed blob: err = %v", err)
	}
}

func TestHandlerMethodAndPathEdges(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)

	if rr := e.serve(t, "alice", http.MethodPatch, AccountsPrefix, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH collection: status = %d", rr.Code)
	}
	one := fmt.Sprintf("%s/%d", AccountsPrefix, id)
	if rr := e.serve(t, "alice", http.MethodPatch, one, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH item: status = %d", rr.Code)
	}
	for _, p := range []string{AccountsPrefix + "/abc", AccountsPrefix + "/0", AccountsPrefix + "/1/extra", AccountsPrefix + "XYZ"} {
		if rr := e.serve(t, "alice", http.MethodGet, p, ""); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", p, rr.Code)
		}
	}
	// The trailing-slash collection alias answers like the bare path.
	if rr := e.serve(t, "alice", http.MethodGet, AccountsPrefix+"/", ""); rr.Code != http.StatusOK {
		t.Errorf("GET trailing slash: status = %d", rr.Code)
	}
}
