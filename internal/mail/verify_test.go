package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// TestVerify exercises Service.Verify directly: the happy path plus both
// sentinel classes, through the REAL imap client against the scripted fake.
func TestVerify(t *testing.T) {
	ctx := context.Background()
	in := AccountInput{
		IMAPHost: "imap.example.com", IMAPPort: 993, IMAPSSLMode: SSLModeSSL,
		IMAPUser: "u", IMAPPassword: "p",
	}

	ok := newIMAPFake(t, func(user, pass string) bool { return user == "u" && pass == "p" })
	svc := &Service{Store: NewSQLStore(testDB(t)), Secret: "s", DialIMAP: ok.dialIMAP}
	if err := svc.Verify(ctx, in); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok.dials.Load() != 1 || ok.logins.Load() != 1 {
		t.Errorf("dials = %d logins = %d, want 1/1", ok.dials.Load(), ok.logins.Load())
	}

	rejecting := newIMAPFake(t, func(string, string) bool { return false })
	svc.DialIMAP = rejecting.dialIMAP
	err := svc.Verify(ctx, in)
	if !errors.Is(err, ErrVerifyAuth) {
		t.Fatalf("Verify with refused LOGIN err = %v, want ErrVerifyAuth", err)
	}
	if errors.Is(err, ErrVerifyConnect) {
		t.Errorf("auth refusal must not double as connect failure: %v", err)
	}
	// The cause (imap.ErrAuthentication) rides inside the wrapper.
	if !errors.Is(err, imap.ErrAuthentication) {
		t.Errorf("Verify err lacks the imap cause: %v", err)
	}

	svc.DialIMAP = func(context.Context, imap.DialOptions) (*imap.Client, error) {
		return nil, errors.New("connection refused")
	}
	err = svc.Verify(ctx, in)
	if !errors.Is(err, ErrVerifyConnect) {
		t.Fatalf("Verify with dead dial err = %v, want ErrVerifyConnect", err)
	}
	if errors.Is(err, ErrVerifyAuth) {
		t.Errorf("connect failure must not double as auth failure: %v", err)
	}
}

// TestHandlerCreateVerifyFailures pins the two distinct 400s (ADR-0108 §4's
// taxonomy): bad credentials and an unreachable server read differently,
// and neither persists a row.
func TestHandlerCreateVerifyFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		dial func(t *testing.T) func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error)
		want string
	}{
		{"auth", func(t *testing.T) func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error) {
			t.Helper()
			return newIMAPFake(t, func(string, string) bool { return false }).dialIMAP
		}, "IMAP authentication failed"},
		{"connect", func(t *testing.T) func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error) {
			t.Helper()
			return func(context.Context, imap.DialOptions) (*imap.Client, error) {
				return nil, errors.New("i/o timeout")
			}
		}, "cannot connect to IMAP server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newMailEnv(t)
			e.svc.DialIMAP = tc.dial(t)
			rr := e.serve(t, "alice", http.MethodPost, AccountsPrefix, createBodyAlice)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s, want 400", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), `"error":"`+tc.want+`"`) {
				t.Errorf("body = %s, want error %q", rr.Body.String(), tc.want)
			}
			if got := e.serve(t, "alice", http.MethodGet, AccountsPrefix, ""); strings.TrimSpace(got.Body.String()) != "[]" {
				t.Errorf("a failed verification persisted a row: %s", got.Body.String())
			}
		})
	}
}

// TestHandlerUpdateReverify pins M2's update rule: a patch touching any of
// {imapHost, imapPort, imapSslMode, imapUser, imapPassword} re-verifies
// against the effective values BEFORE persisting; other patches never dial.
func TestHandlerUpdateReverify(t *testing.T) {
	e := newMailEnv(t)
	id := createAlice(t, e)
	one := fmt.Sprintf("%s/%d", AccountsPrefix, id)
	if e.fake.dials.Load() != 1 {
		t.Fatalf("dials after create = %d, want 1", e.fake.dials.Load())
	}

	// A name-only patch is not an IMAP connection change: no dial.
	rr := e.serve(t, "alice", http.MethodPut, one, `{"name":"No dial"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("name-only update: status = %d", rr.Code)
	}
	if e.fake.dials.Load() != 1 {
		t.Errorf("dials after name-only update = %d, want 1", e.fake.dials.Load())
	}

	// A password patch re-verifies (the fake accepts the new password).
	rr = e.serve(t, "alice", http.MethodPut, one, `{"imapPassword":"imap-secret-pw-2"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("password update: status = %d body = %s", rr.Code, rr.Body.String())
	}
	if e.fake.dials.Load() != 2 {
		t.Errorf("dials after password update = %d, want 2", e.fake.dials.Load())
	}

	// A port patch against an unreachable server fails 400 and persists
	// nothing: the row keeps the old port.
	e.svc.DialIMAP = func(context.Context, imap.DialOptions) (*imap.Client, error) {
		return nil, errors.New("connection refused")
	}
	rr = e.serve(t, "alice", http.MethodPut, one, `{"imapPort":143}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "cannot connect to IMAP server") {
		t.Fatalf("port update: status = %d body = %s", rr.Code, rr.Body.String())
	}
	row, err := e.store.GetByID(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if row.IMAPPort != 993 {
		t.Errorf("imapPort after failed re-verify = %d, want 993 (no persist)", row.IMAPPort)
	}

	// An auth-refused host move is the other distinct 400; the stored host
	// (and the still-opening blob) stay put.
	e.svc.DialIMAP = newIMAPFake(t, func(string, string) bool { return false }).dialIMAP
	rr = e.serve(t, "alice", http.MethodPut, one, `{"imapHost":"imap.other.com"}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "IMAP authentication failed") {
		t.Fatalf("host update: status = %d body = %s", rr.Code, rr.Body.String())
	}
	row, err = e.store.GetByID(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if row.IMAPHost != "imap.example.com" {
		t.Errorf("imapHost after failed re-verify = %s, want unchanged", row.IMAPHost)
	}
	if _, _, err := e.svc.passwords("alice", row.IMAPHost, row.IMAPUser, row.PasswordSealed); err != nil {
		t.Errorf("blob no longer opens after failed re-verify: %v", err)
	}
}
