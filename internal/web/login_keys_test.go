package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// spyKeys is a LoginKeyHandler fake recording its calls.
type spyKeys struct {
	unlockCalls [][2]string
	priv        []byte
	unlockErr   error
	sealCalls   int
	sealErr     error
}

func (s *spyKeys) UnlockForLogin(_ context.Context, uid, password string) ([]byte, error) {
	s.unlockCalls = append(s.unlockCalls, [2]string{uid, password})
	if s.unlockErr != nil {
		return nil, s.unlockErr
	}
	return s.priv, nil
}

func (s *spyKeys) SealSessionKey(priv []byte, sessionID string) ([]byte, error) {
	s.sealCalls++
	if s.sealErr != nil {
		return nil, s.sealErr
	}
	return append([]byte("sealed:"), priv...), nil
}

// methodVerifier returns a basic principal for the account password and an
// app-password principal for the app token, mimicking the production chain.
type methodVerifier struct{}

func (methodVerifier) Verify(_ context.Context, user, pass string) (*auth.Principal, error) {
	switch {
	case user == "alice" && pass == "wonderland":
		return &auth.Principal{UID: user, Enabled: true, AuthMethod: auth.AuthMethodBasic}, nil
	case user == "alice" && pass == "app-token":
		return &auth.Principal{UID: user, Enabled: true, AuthMethod: auth.AuthMethodAppPassword}, nil
	}
	return nil, auth.ErrInvalidCredentials
}

// loginKeysRig wires BrowserLogin like loginRig but with a spy key handler
// and a verifier that can mint app-password principals.
type loginKeysRig struct {
	db       database.DB
	sessions *session.SQLStore
	tokens   *auth.RequestToken
	handler  *BrowserLogin
	keys     *spyKeys
	userID   int64
}

// testWebDB opens a migrated in-memory sqlite, mirroring newLoginRig's
// setup.
func testWebDB(t *testing.T) database.DB {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedWebUser(t *testing.T, db database.DB, uid string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(ctx, `INSERT INTO users (uid, display_name, password_hash, enabled, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
		uid, uid, "x", now, now); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow(ctx, `SELECT id FROM users WHERE uid = ?`, uid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func newLoginKeysRig(t *testing.T) *loginKeysRig {
	t.Helper()
	db := testWebDB(t)
	userID := seedWebUser(t, db, "alice")
	tokens := auth.NewRequestToken("rig-secret")
	sessions := session.NewSQLStore(db)
	keys := &spyKeys{priv: []byte("0123456789abcdef0123456789abcdef")}
	rig := &loginKeysRig{db: db, sessions: sessions, tokens: tokens, keys: keys, userID: userID}
	rig.handler = &BrowserLogin{
		Verifier: methodVerifier{},
		Users:    stubUserStore{u: &users.User{ID: userID, UID: "alice", DisplayName: "Alice", Enabled: true}},
		Sessions: sessions,
		Tokens:   tokens,
		Keys:     keys,
	}
	return rig
}

func (rig *loginKeysRig) postLogin(t *testing.T, password string) *httptest.ResponseRecorder {
	t.Helper()
	const nonce = "login-nonce"
	form := url.Values{
		"user":                  {"alice"},
		"password":              {password},
		auth.RequestTokenHeader: {rig.tokens.DeriveLoginToken(nonce)},
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: LoginNonceCookie, Value: nonce})
	rr := httptest.NewRecorder()
	rig.handler.HandleLogin(rr, req)
	return rr
}

func (rig *loginKeysRig) sessionCookie(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == session.CookieName {
			return c.Value
		}
	}
	t.Fatal("no session cookie set")
	return ""
}

// TestBrowserLoginAttachesSealedUK pins the ADR-0100 login path: a basic
// login unlocks and stores the sealed key copy on the session row; an
// app-password form login never calls the key handler and stores nothing.
func TestBrowserLoginAttachesSealedUK(t *testing.T) {
	ctx := context.Background()
	rig := newLoginKeysRig(t)

	rr := rig.postLogin(t, "wonderland")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("basic login = %d, want 303", rr.Code)
	}
	if len(rig.keys.unlockCalls) != 1 || rig.keys.unlockCalls[0] != [2]string{"alice", "wonderland"} {
		t.Fatalf("unlock calls = %v", rig.keys.unlockCalls)
	}
	if rig.keys.sealCalls != 1 {
		t.Fatalf("seal calls = %d, want 1", rig.keys.sealCalls)
	}
	sid := rig.sessionCookie(t, rr)
	sess, err := rig.sessions.Get(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte("sealed:"), rig.keys.priv...)
	if string(sess.SealedUK) != string(want) {
		t.Errorf("session sealed_uk = %q, want %q", sess.SealedUK, want)
	}

	// App password at the same form: no unlock, no sealed copy.
	rig.keys.unlockCalls = nil
	rig.keys.sealCalls = 0
	rr = rig.postLogin(t, "app-token")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("app-password login = %d, want 303", rr.Code)
	}
	if len(rig.keys.unlockCalls) != 0 {
		t.Errorf("unlock called for an app-password login: %v", rig.keys.unlockCalls)
	}
	sid = rig.sessionCookie(t, rr)
	sess, err = rig.sessions.Get(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if sess.SealedUK != nil {
		t.Errorf("app-password session sealed_uk = %q, want nil", sess.SealedUK)
	}
}

// TestBrowserLoginKeyFailures pins the loud-failure semantics: an unlock
// error is a 500 with no session; a seal/store error is a 500 and the
// created session is deleted (a keyless session would 403 every file).
func TestBrowserLoginKeyFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("unlock error", func(t *testing.T) {
		rig := newLoginKeysRig(t)
		rig.keys.unlockErr = errors.New("password unwrap failed")
		rr := rig.postLogin(t, "wonderland")
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("unlock failure = %d, want 500", rr.Code)
		}
		var n int64
		if err := rig.db.QueryRow(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("sessions after failed unlock = %d, want 0", n)
		}
	})

	t.Run("store error deletes the session", func(t *testing.T) {
		rig := newLoginKeysRig(t)
		failing := &failSetSealedUK{Store: rig.sessions}
		rig.handler.Sessions = failing
		rr := rig.postLogin(t, "wonderland")
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("store failure = %d, want 500", rr.Code)
		}
		var n int64
		if err := rig.db.QueryRow(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("sessions after failed key store = %d, want 0 (session deleted)", n)
		}
	})
}

// failSetSealedUK delegates everything but SetSealedUK, which fails.
type failSetSealedUK struct{ session.Store }

func (failSetSealedUK) SetSealedUK(context.Context, string, []byte) error {
	return errors.New("set sealed uk: db gone")
}

// TestLoginV2GrantCopiesUnlockedKey pins the login-v2 side: the
// basic-authenticated grant request unlocks, and the issued session carries
// the sealed copy; a seal/store failure deletes the session and 500s.
func TestLoginV2GrantCopiesUnlockedKey(t *testing.T) {
	grant := func(t *testing.T, h *LoginV2) *httptest.ResponseRecorder {
		t.Helper()
		flow, err := h.Service.Init(context.Background(), "test client")
		if err != nil {
			t.Fatal(err)
		}
		st, err := h.Service.BeginGrant(context.Background(), flow.LoginToken)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.NewReader(url.Values{"stateToken": {st.StateToken}}.Encode())
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/index.php/login/v2/grant", body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", basicAuth("alice", "wonderland"))
		rr := httptest.NewRecorder()
		h.HandleGrant(rr, req)
		return rr
	}
	newV2 := func(t *testing.T, keys *spyKeys, ss *stubSessionStore) *LoginV2 {
		t.Helper()
		h := newHandler(t, stubIssuer{password: "app-pw-grant"})
		h.Users = stubUserStore{u: &users.User{ID: 7, UID: "alice", DisplayName: "Alice", Enabled: true}}
		h.Sessions = ss
		h.Keys = keys
		return h
	}

	t.Run("basic grant unlocks and copies", func(t *testing.T) {
		keys := &spyKeys{priv: []byte("0123456789abcdef0123456789abcdef")}
		ss := &stubSessionStore{}
		rr := grant(t, newV2(t, keys, ss))
		if rr.Code != http.StatusOK {
			t.Fatalf("grant = %d, want 200", rr.Code)
		}
		if len(keys.unlockCalls) != 1 || keys.unlockCalls[0] != [2]string{"alice", "wonderland"} {
			t.Fatalf("unlock calls = %v", keys.unlockCalls)
		}
		want := append([]byte("sealed:"), keys.priv...)
		if string(ss.sealedUK) != string(want) {
			t.Errorf("session sealed_uk = %q, want %q", ss.sealedUK, want)
		}
		if len(ss.deleted) != 0 {
			t.Errorf("session deleted on success: %v", ss.deleted)
		}
	})

	t.Run("store error deletes the session and 500s", func(t *testing.T) {
		keys := &spyKeys{priv: []byte("0123456789abcdef0123456789abcdef")}
		ss := &stubSessionStore{setErr: errors.New("db gone")}
		rr := grant(t, newV2(t, keys, ss))
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("grant with failing store = %d, want 500", rr.Code)
		}
		if len(ss.deleted) != 1 || ss.deleted[0] != ss.created.ID {
			t.Errorf("deleted = %v, want the created session", ss.deleted)
		}
	})

	t.Run("no keys wired stores nothing", func(t *testing.T) {
		ss := &stubSessionStore{}
		h := newHandler(t, stubIssuer{password: "app-pw-grant"})
		h.Users = stubUserStore{u: &users.User{ID: 7, UID: "alice", DisplayName: "Alice", Enabled: true}}
		h.Sessions = ss
		// h.Keys stays nil (a typed-nil *spyKeys would widen to a non-nil
		// interface — exactly the trap app routes.go guards).
		rr := grant(t, h)
		if rr.Code != http.StatusOK {
			t.Fatalf("grant = %d, want 200", rr.Code)
		}
		if ss.sealedUK != nil {
			t.Errorf("sealed_uk without Keys = %q, want nil", ss.sealedUK)
		}
	})
}
