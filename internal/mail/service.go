package mail

import (
	"context"
	"fmt"
)

// Service is the mail account business layer: it owns credential sealing
// (ADR-0108 §3) so the store only ever handles the sealed blob. Secret is
// the instance secret the app wiring resolves (generated at boot when
// unconfigured); the seal is independent of the per-user-keys encryption
// module so background sync (M2+) can open credentials without any user's
// unlocked key.
type Service struct {
	Store  Store
	Secret string
}

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

// Create seals the password pair (an empty SMTP password means "same as
// IMAP") and stores the account for uid.
func (s *Service) Create(ctx context.Context, uid string, in AccountInput) (*Account, error) {
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
// (500), never a silent credential wipe.
func (s *Service) Update(ctx context.Context, uid string, id int64, patch AccountPatch) (*Account, error) {
	a, err := s.Store.GetByID(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	oldHost, oldUser := a.IMAPHost, a.IMAPUser
	applyPatch(a, patch)
	if patch.IMAPPassword != nil || patch.SMTPPassword != nil || oldHost != a.IMAPHost || oldUser != a.IMAPUser {
		imap, smtp, err := s.passwords(a.UserID, oldHost, oldUser, a.PasswordSealed)
		if err != nil {
			return nil, err
		}
		if patch.IMAPPassword != nil {
			imap = *patch.IMAPPassword
		}
		if patch.SMTPPassword != nil {
			smtp = *patch.SMTPPassword
		}
		packed, err := joinPasswords(imap, smtp)
		if err != nil {
			return nil, err
		}
		sealed, err := SealCredential(s.Secret, a.UserID, a.IMAPHost, a.IMAPUser, packed)
		if err != nil {
			return nil, err
		}
		a.PasswordSealed = sealed
	}
	if err := s.Store.Update(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) Delete(ctx context.Context, uid string, id int64) error {
	return s.Store.Delete(ctx, uid, id)
}

// passwords opens a sealed blob into its IMAP/SMTP halves; an open failure
// wraps ErrCredential (integrity, fail-closed).
func (s *Service) passwords(userID, imapHost, imapUser string, sealed []byte) (imap, smtp string, err error) {
	packed, err := OpenCredential(s.Secret, userID, imapHost, imapUser, sealed)
	if err != nil {
		return "", "", err
	}
	imap, smtp, err = splitPasswords(packed)
	if err != nil {
		return "", "", fmt.Errorf("mail: open passwords: %w", err)
	}
	return imap, smtp, nil
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
