package wopi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

// fakeTokenKeys is a TokenKeyWrapper + TokenKeyCleaner spy: it records every
// call and returns the canned wrap/unlock results.
type fakeTokenKeys struct {
	wrapCalls   []wrapCall
	unlockCalls []unlockCall
	deleted     [][]string
	pruneCalls  int
	wrapErr     error
	unlockPriv  []byte
	unlockErr   error
	deleteErr   error
	pruneErr    error
}

type wrapCall struct {
	token  string
	fileID int64
	priv   []byte
}

type unlockCall struct {
	token  string
	fileID int64
}

func (f *fakeTokenKeys) WrapKeyForWOPIToken(_ context.Context, tokenRaw string, fileID int64, priv []byte) error {
	f.wrapCalls = append(f.wrapCalls, wrapCall{token: tokenRaw, fileID: fileID, priv: priv})
	return f.wrapErr
}

func (f *fakeTokenKeys) UnlockForWOPIToken(_ context.Context, tokenRaw string, fileID int64) ([]byte, error) {
	f.unlockCalls = append(f.unlockCalls, unlockCall{token: tokenRaw, fileID: fileID})
	return f.unlockPriv, f.unlockErr
}

func (f *fakeTokenKeys) DeleteWOPITokenKeys(_ context.Context, tokens []string) (int64, error) {
	f.deleted = append(f.deleted, tokens)
	return int64(len(tokens)), f.deleteErr
}

func (f *fakeTokenKeys) PruneWOPITokenKeys(context.Context) (int64, error) {
	f.pruneCalls++
	return 0, f.pruneErr
}

// withUserKey attaches a principal carrying a 32-byte unlocked key (an
// enrolled user's session, ADR-0100).
func withUserKey(r *http.Request, uid string, key []byte) *http.Request {
	return r.WithContext(auth.WithUser(r.Context(), &auth.Principal{UID: uid, Enabled: true, UnlockedKey: key}))
}

func (e *wopiEnv) writeDoc(t *testing.T) int64 {
	t.Helper()
	if _, _, err := e.dav.Write(t.Context(), "alice", "/doc.odt", bytes.NewReader([]byte("hello wopi")), nil); err != nil {
		t.Fatal(err)
	}
	return e.fileID(t, "/doc.odt")
}

// TestMintWrapsKey pins wrap-at-mint (ADR-0107): a mint whose session
// principal carries a 32-byte unlocked key seals it under the new token with
// the exact (token, fileID, priv) triple.
func TestMintWrapsKey(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	id := env.writeDoc(t)
	keys := &fakeTokenKeys{}
	env.svc.Keys = keys
	priv := bytes.Repeat([]byte{0x42}, 32)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/index.php/apps/richdocuments/wopi/token?fileId="+strconv.FormatInt(id, 10), nil)
	env.mint.ServeHTTP(rr, withUserKey(req, "alice", priv))
	if rr.Code != http.StatusOK {
		t.Fatalf("mint = %d body=%s", rr.Code, rr.Body.String())
	}
	if len(keys.wrapCalls) != 1 {
		t.Fatalf("wrap calls = %d, want 1", len(keys.wrapCalls))
	}
	call := keys.wrapCalls[0]
	if call.fileID != id {
		t.Errorf("wrap fileID = %d, want %d", call.fileID, id)
	}
	if !bytes.Equal(call.priv, priv) {
		t.Error("wrap priv differs from the session's unlocked key")
	}
	tok, err := env.store.GetByToken(ctx, call.token, env.freeze)
	if err != nil {
		t.Fatalf("wrapped token not in the store: %v", err)
	}
	if tok.UID != "alice" || tok.FileID != id {
		t.Errorf("stored token = %+v", tok)
	}
}

// TestMintWrapFailureRollsBack pins the LOUD wrap failure (mirroring
// login_v2's grant): a wrap error fails the mint with 500 AND removes the
// token row — a silently unwrapped token would 403 every callback.
func TestMintWrapFailureRollsBack(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	id := env.writeDoc(t)
	keys := &fakeTokenKeys{wrapErr: errors.New("db gone")}
	env.svc.Keys = keys
	env.svc.NewToken = func() string { return "tok-rolled-back" }

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/index.php/apps/richdocuments/wopi/token?fileId="+strconv.FormatInt(id, 10), nil)
	env.mint.ServeHTTP(rr, withUserKey(req, "alice", bytes.Repeat([]byte{0x42}, 32)))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("mint = %d body=%s, want 500", rr.Code, rr.Body.String())
	}
	if _, err := env.store.GetByToken(ctx, "tok-rolled-back", env.freeze); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("token row after failed wrap = %v, want ErrTokenNotFound", err)
	}
}

// TestMintSkipsWrapWithoutKey pins the pinned no-wrap behavior (ADR-0102
// §3): no Keys wiring, no unlocked key on the principal, or a wrong-length
// key all mint WITHOUT a wrap call — such tokens authenticate but their
// callbacks keep the 403 boundary.
func TestMintSkipsWrapWithoutKey(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	id := env.writeDoc(t)

	// Keys nil (default env): mint succeeds, nothing to record.
	if m := env.mintToken(t, "alice", id); m.Token == "" {
		t.Fatal("mint without Keys returned an empty token")
	}

	keys := &fakeTokenKeys{}
	env.svc.Keys = keys
	// Keyless principal (app-password-auth minter, unenrolled user).
	if m := env.mintToken(t, "alice", id); m.Token == "" {
		t.Fatal("mint with a keyless principal returned an empty token")
	}
	// A wrong-length key is not a usable unlocked key.
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/index.php/apps/richdocuments/wopi/token?fileId="+strconv.FormatInt(id, 10), nil)
	env.mint.ServeHTTP(rr, withUserKey(req, "alice", []byte("short")))
	if rr.Code != http.StatusOK {
		t.Fatalf("mint with a short key = %d body=%s", rr.Code, rr.Body.String())
	}
	if len(keys.wrapCalls) != 0 {
		t.Errorf("wrap calls = %d, want 0", len(keys.wrapCalls))
	}
}

// TestCallbackUnlocksKey pins the anonymous-callback side (ADR-0107): the
// files handler opens the token's wrap with the exact (token, fileID) pair
// and the request proceeds with the unlocked key attached; a wrap error is
// fail-closed 500 (NEVER a silent keyless 403); a nil priv keeps the
// anonymous flow.
func TestCallbackUnlocksKey(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	id := env.writeDoc(t)
	m := env.mintToken(t, "alice", id)

	keys := &fakeTokenKeys{unlockPriv: bytes.Repeat([]byte{0x42}, 32)}
	env.svc.Keys = keys
	rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet,
		filesURL(id, false)+"?access_token="+m.Token, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("CheckFileInfo = %d body=%s", rr.Code, rr.Body.String())
	}
	if len(keys.unlockCalls) != 1 {
		t.Fatalf("unlock calls = %d, want 1", len(keys.unlockCalls))
	}
	call := keys.unlockCalls[0]
	if call.token != m.Token || call.fileID != id {
		t.Errorf("unlock call = (%q, %d), want (%q, %d)", call.token, call.fileID, m.Token, id)
	}

	// Fail-closed: a wrap error is a 500, never a keyless fallback.
	keys.unlockErr = errors.New("wrap authentication failed")
	rr = env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet,
		filesURL(id, false)+"?access_token="+m.Token, nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("wrap error: CheckFileInfo = %d, want 500", rr.Code)
	}

	// No wrap row (nil priv, nil err): the normal anonymous flow continues.
	keys.unlockPriv, keys.unlockErr = nil, nil
	rr = env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet,
		filesURL(id, false)+"?access_token="+m.Token, nil))
	if rr.Code != http.StatusOK {
		t.Errorf("nil wrap: CheckFileInfo = %d body=%s, want 200 (anonymous flow)", rr.Code, rr.Body.String())
	}
}

// TestGCDeletesTokenKeyWraps pins the sweep order (ADR-0107): the reaped
// tokens' wraps go first (exactly the expired set), then the token rows,
// then the orphan prune; live tokens and their wraps stay.
func TestGCDeletesTokenKeyWraps(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if err := env.store.Insert(ctx, &Token{Token: "tok-expired-1", UID: "alice", FileID: 1, CanWrite: true, ExpiresAt: env.freeze.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Insert(ctx, &Token{Token: "tok-expired-2", UID: "alice", FileID: 2, ExpiresAt: env.freeze.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Insert(ctx, &Token{Token: "tok-live", UID: "alice", FileID: 3, CanWrite: true, ExpiresAt: env.freeze.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	keys := &fakeTokenKeys{}
	job := NewGCJob(env.store, env.svc.Clock, keys, nil)
	if err := job.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(keys.deleted) != 1 {
		t.Fatalf("DeleteWOPITokenKeys calls = %d, want 1", len(keys.deleted))
	}
	got := keys.deleted[0]
	if len(got) != 2 {
		t.Fatalf("deleted tokens = %v, want the two expired", got)
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen["tok-expired-1"] || !seen["tok-expired-2"] {
		t.Errorf("deleted tokens = %v, want [tok-expired-1 tok-expired-2]", got)
	}
	if keys.pruneCalls != 1 {
		t.Errorf("prune calls = %d, want 1", keys.pruneCalls)
	}
	if _, err := env.store.GetByToken(ctx, "tok-expired-1", env.freeze); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expired token still present: %v", err)
	}
	if _, err := env.store.GetByToken(ctx, "tok-live", env.freeze); err != nil {
		t.Errorf("live token deleted: %v", err)
	}
}

// TestGCKeyErrorsNeverFailSweep pins the best-effort key side: list, delete,
// and prune failures are Warn-logged and the token rows are still reaped.
func TestGCKeyErrorsNeverFailSweep(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if err := env.store.Insert(ctx, &Token{Token: "tok-expired", UID: "alice", FileID: 1, ExpiresAt: env.freeze.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	keys := &fakeTokenKeys{deleteErr: errors.New("keys db gone"), pruneErr: errors.New("prune gone")}
	job := NewGCJob(env.store, env.svc.Clock, keys, nil)
	if err := job.Run(ctx, nil); err != nil {
		t.Fatalf("Run = %v, want nil despite key-side errors", err)
	}
	if len(keys.deleted) != 1 || keys.pruneCalls != 1 {
		t.Errorf("delete calls = %d prune calls = %d, want 1 and 1", len(keys.deleted), keys.pruneCalls)
	}
	if _, err := env.store.GetByToken(ctx, "tok-expired", env.freeze); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expired token still present: %v", err)
	}

	// Nil keys: the sweep is store-only (pre-ADR-0107 behavior).
	if err := env.store.Insert(ctx, &Token{Token: "tok-expired-2", UID: "alice", FileID: 2, ExpiresAt: env.freeze.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	job = NewGCJob(env.store, env.svc.Clock, nil, nil)
	if err := job.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.GetByToken(ctx, "tok-expired-2", env.freeze); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expired token still present with nil keys: %v", err)
	}
}
