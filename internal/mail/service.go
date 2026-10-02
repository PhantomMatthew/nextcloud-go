package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// Service is the mail account business layer: it owns credential sealing
// (ADR-0108 §3) so the store only ever handles the sealed blob, and it
// verifies the IMAP LOGIN before an account is persisted (M2, ADR-0108
// §1/§4). Secret is the instance secret the app wiring resolves (generated
// at boot when unconfigured); the seal is independent of the per-user-keys
// encryption module so background sync (M3+) can open credentials without
// any user's unlocked key. DialIMAP is the dial seam: nil falls back to
// imap.Dial, production wiring installs the egress-guarded closure, tests
// inject scripted fakes. Logger is optional (nil-safe).
type Service struct {
	Store    Store
	Secret   string
	DialIMAP func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error)
	Logger   *slog.Logger
}

// Verify-on-create sentinels (ADR-0108 §4's failure taxonomy): both wrap
// the underlying cause, so errors.Is finds the class and the cause.
var (
	// ErrVerifyAuth reports a verification dial whose IMAP LOGIN was
	// refused (tagged NO): the server rejected the credentials.
	ErrVerifyAuth = errors.New("mail: imap verification: authentication failed")
	// ErrVerifyConnect reports a verification that never completed a LOGIN
	// exchange: unreachable host, timeout, TLS failure, or protocol error.
	ErrVerifyConnect = errors.New("mail: imap verification: cannot connect")
)

// AccountInput carries every field of a new account; the handler validates
// the request shape (400s) before this runs.
type AccountInput struct {
	Name         string
	Email        string
	IMAPHost     string
	IMAPPort     int
	IMAPSSLMode  string
	IMAPUser     string
	IMAPPassword string
	SMTPHost     string
	SMTPPort     int
	SMTPSSLMode  string
	SMTPUser     string
	SMTPPassword string
}

// AccountPatch is a partial update: nil fields keep their stored values.
// Password fields re-seal only when non-nil.
type AccountPatch struct {
	Name         *string
	Email        *string
	IMAPHost     *string
	IMAPPort     *int
	IMAPSSLMode  *string
	IMAPUser     *string
	IMAPPassword *string
	SMTPHost     *string
	SMTPPort     *int
	SMTPSSLMode  *string
	SMTPUser     *string
	SMTPPassword *string
}

// Create verifies the IMAP LOGIN (M2: a failed verification persists no
// row), then seals the password pair (an empty SMTP password means "same
// as IMAP") and stores the account for uid.
func (s *Service) Create(ctx context.Context, uid string, in AccountInput) (*Account, error) {
	if err := s.Verify(ctx, in); err != nil {
		return nil, err
	}
	smtp := in.SMTPPassword
	if smtp == "" {
		smtp = in.IMAPPassword
	}
	packed, err := joinPasswords(in.IMAPPassword, smtp)
	if err != nil {
		return nil, err
	}
	sealed, err := SealCredential(s.Secret, uid, in.IMAPHost, in.IMAPUser, packed)
	if err != nil {
		return nil, err
	}
	a := &Account{
		UserID:         uid,
		Name:           in.Name,
		Email:          in.Email,
		IMAPHost:       in.IMAPHost,
		IMAPPort:       in.IMAPPort,
		IMAPSSLMode:    in.IMAPSSLMode,
		IMAPUser:       in.IMAPUser,
		SMTPHost:       in.SMTPHost,
		SMTPPort:       in.SMTPPort,
		SMTPSSLMode:    in.SMTPSSLMode,
		SMTPUser:       in.SMTPUser,
		PasswordSealed: sealed,
	}
	if err := s.Store.Create(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) List(ctx context.Context, uid string) ([]Account, error) {
	return s.Store.ListByUser(ctx, uid)
}

func (s *Service) Get(ctx context.Context, uid string, id int64) (*Account, error) {
	return s.Store.GetByID(ctx, uid, id)
}

// Update applies patch to the account (uid, id). The sealed blob is
// re-sealed when a password half is provided OR when the identity the blob
// is bound to (imap host/user) moves — the AD would otherwise strand the
// credential. A stored blob that fails to open is a loud integrity error
// (500), never a silent credential wipe. M2: when any of the IMAP
// connection fields (host, port, ssl mode, user, password) changes, the
// account is re-verified against the EFFECTIVE values before anything
// persists — the password comes from the patch when present, else from the
// sealed blob opened under the OLD identity triple.
func (s *Service) Update(ctx context.Context, uid string, id int64, patch AccountPatch) (*Account, error) {
	a, err := s.Store.GetByID(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	oldHost, oldUser := a.IMAPHost, a.IMAPUser
	oldPort, oldMode := a.IMAPPort, a.IMAPSSLMode
	applyPatch(a, patch)
	verify := a.IMAPHost != oldHost || a.IMAPUser != oldUser ||
		a.IMAPPort != oldPort || a.IMAPSSLMode != oldMode || patch.IMAPPassword != nil
	rekey := patch.IMAPPassword != nil || patch.SMTPPassword != nil ||
		oldHost != a.IMAPHost || oldUser != a.IMAPUser
	if verify || rekey {
		imapPW, smtpPW, err := s.passwords(a.UserID, oldHost, oldUser, a.PasswordSealed)
		if err != nil {
			return nil, err
		}
		if patch.IMAPPassword != nil {
			imapPW = *patch.IMAPPassword
		}
		if patch.SMTPPassword != nil {
			smtpPW = *patch.SMTPPassword
		}
		if verify {
			if err := s.Verify(ctx, AccountInput{
				IMAPHost:     a.IMAPHost,
				IMAPPort:     a.IMAPPort,
				IMAPSSLMode:  a.IMAPSSLMode,
				IMAPUser:     a.IMAPUser,
				IMAPPassword: imapPW,
			}); err != nil {
				return nil, err
			}
		}
		if rekey {
			packed, err := joinPasswords(imapPW, smtpPW)
			if err != nil {
				return nil, err
			}
			sealed, err := SealCredential(s.Secret, a.UserID, a.IMAPHost, a.IMAPUser, packed)
			if err != nil {
				return nil, err
			}
			a.PasswordSealed = sealed
		}
	}
	if err := s.Store.Update(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// Verify checks the account's IMAP end before it is persisted: dial through
// the DialIMAP seam (production: egress-guarded), LOGIN, LOGOUT. A refused
// LOGIN is ErrVerifyAuth; every other failure — dial, timeout, TLS,
// protocol — is ErrVerifyConnect. Both wrap the underlying cause.
func (s *Service) Verify(ctx context.Context, in AccountInput) error {
	dial := s.DialIMAP
	if dial == nil {
		dial = imap.Dial
	}
	client, err := dial(ctx, imap.DialOptions{
		Host:    in.IMAPHost,
		Port:    in.IMAPPort,
		SSLMode: in.IMAPSSLMode,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVerifyConnect, err)
	}
	if err := client.Login(in.IMAPUser, in.IMAPPassword); err != nil {
		// The LOGIN answer decides the mapping; LOGOUT's own outcome cannot
		// change it, so it rides along as joined diagnostics only.
		return errors.Join(mapVerifyError(err), client.Logout())
	}
	if err := client.Logout(); err != nil && s.Logger != nil {
		// Verification already succeeded; LOGOUT is best-effort (ADR-0108
		// §1) and a rude reply cannot un-verify the account.
		s.Logger.DebugContext(ctx, "mail: verify: logout failed after a successful login", slog.String("error", err.Error()))
	}
	return nil
}

// mapVerifyError classes a LOGIN failure: a credential refusal is
// ErrVerifyAuth, everything else ErrVerifyConnect.
func mapVerifyError(err error) error {
	if errors.Is(err, imap.ErrAuthentication) {
		return fmt.Errorf("%w: %w", ErrVerifyAuth, err)
	}
	return fmt.Errorf("%w: %w", ErrVerifyConnect, err)
}

func (s *Service) Delete(ctx context.Context, uid string, id int64) error {
	return s.Store.Delete(ctx, uid, id)
}

// passwords opens a sealed blob into its IMAP/SMTP halves; an open failure
// wraps ErrCredential (integrity, fail-closed).
func (s *Service) passwords(userID, imapHost, imapUser string, sealed []byte) (imap, smtp string, err error) {
	return openPasswords(s.Secret, userID, imapHost, imapUser, sealed)
}

// openPasswords is the passwords logic shared with the M3 sync engine,
// which opens credentials in a job ctx with no Service at hand.
func openPasswords(secret, userID, imapHost, imapUser string, sealed []byte) (imapPW, smtpPW string, err error) {
	packed, err := OpenCredential(secret, userID, imapHost, imapUser, sealed)
	if err != nil {
		return "", "", err
	}
	imapPW, smtpPW, err = splitPasswords(packed)
	if err != nil {
		return "", "", fmt.Errorf("mail: open passwords: %w", err)
	}
	return imapPW, smtpPW, nil
}

func applyPatch(a *Account, p AccountPatch) {
	if p.Name != nil {
		a.Name = *p.Name
	}
	if p.Email != nil {
		a.Email = *p.Email
	}
	if p.IMAPHost != nil {
		a.IMAPHost = *p.IMAPHost
	}
	if p.IMAPPort != nil {
		a.IMAPPort = *p.IMAPPort
	}
	if p.IMAPSSLMode != nil {
		a.IMAPSSLMode = *p.IMAPSSLMode
	}
	if p.IMAPUser != nil {
		a.IMAPUser = *p.IMAPUser
	}
	if p.SMTPHost != nil {
		a.SMTPHost = *p.SMTPHost
	}
	if p.SMTPPort != nil {
		a.SMTPPort = *p.SMTPPort
	}
	if p.SMTPSSLMode != nil {
		a.SMTPSSLMode = *p.SMTPSSLMode
	}
	if p.SMTPUser != nil {
		a.SMTPUser = *p.SMTPUser
	}
}
