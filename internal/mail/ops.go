package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// ops.go is the M4 live-op layer (ADR-0108 §6): every method runs its IMAP
// work over ONE per-request connection (dial → LOGIN → SELECT → LOGOUT) —
// v1 has no connection pooling (a documented follow-up), and the sync
// engine stays the only long-lived IMAP consumer. The local summary rows
// converge through the same store the sync engine writes: flag writes
// update the row immediately, and a delete/move removes it (a moved message
// is rediscovered in its destination by the next sync's uid diff).

var (
	// ErrMessageGone reports a message (or mailbox) the server no longer
	// has: UID FETCH returned nothing, or SELECT/STORE/COPY was refused
	// with a tagged NO → 404.
	ErrMessageGone = errors.New("mail: message gone on the server")
	// ErrUpstream reports a dial/login/transport failure against the IMAP
	// server → 502 (the account exists and the request was valid).
	ErrUpstream = errors.New("mail: imap upstream failure")
)

// MessageOps runs the M4 live IMAP operations. Store is the mail Store;
// Secret is the instance secret the credentials seal under; DialIMAP is the
// dial seam (nil → imap.Dial, production installs the egress-guarded
// closure); Logger is optional (nil-safe). It holds no per-request state,
// so concurrent requests are safe (each dials its own connection).
type MessageOps struct {
	Store    Store
	Secret   string
	DialIMAP func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error)
	Logger   *slog.Logger
}

// FetchRaw returns the whole RFC 822 message (BODY.PEEK[] — the fetch
// itself never touches flags).
func (o *MessageOps) FetchRaw(ctx context.Context, a *Account, mb *Mailbox, msg *Message) ([]byte, error) {
	sess, err := o.openSession(ctx, a, mb)
	if err != nil {
		return nil, err
	}
	defer sess.close(ctx)
	return sess.fetchRaw(ctx, msg)
}

// Detail fetches the whole message and — when markSeen — marks it \Seen
// (server + local row), all over one connection. It returns the raw bytes
// and the message's flag tokens AFTER any mark (the detail response renders
// those, so a first read shows \Seen immediately).
func (o *MessageOps) Detail(ctx context.Context, a *Account, mb *Mailbox, msg *Message, markSeen bool) ([]byte, []string, error) {
	sess, err := o.openSession(ctx, a, mb)
	if err != nil {
		return nil, nil, err
	}
	defer sess.close(ctx)
	raw, err := sess.fetchRaw(ctx, msg)
	if err != nil {
		return nil, nil, err
	}
	flags := strings.Fields(msg.Flags)
	if markSeen {
		if flags, err = sess.storeFlags(ctx, mb, msg, []string{`\Seen`}, nil); err != nil {
			return nil, nil, err
		}
	}
	return raw, flags, nil
}

// StoreFlags applies the add/remove flag sets server-side (UID STORE — the
// imap layer allowlist-validates before any write) and rewrites the local
// row's flags, returning the updated tokens.
func (o *MessageOps) StoreFlags(ctx context.Context, a *Account, mb *Mailbox, msg *Message, add, remove []string) ([]string, error) {
	sess, err := o.openSession(ctx, a, mb)
	if err != nil {
		return nil, err
	}
	defer sess.close(ctx)
	return sess.storeFlags(ctx, mb, msg, add, remove)
}

// RemoveMessage deletes msg from its mailbox server-side and lands the
// local half: when dest is non-nil the message is UID COPYied there first
// (the trash flow and move share this), then \Deleted + EXPUNGE; the local
// row is deleted either way — after a move the row is deliberately NOT
// re-pointed at the destination (the server assigns the copy a NEW uid), so
// the next sync's uid diff rediscovers it there.
func (o *MessageOps) RemoveMessage(ctx context.Context, a *Account, src, dest *Mailbox, msg *Message) error {
	sess, err := o.openSession(ctx, a, src)
	if err != nil {
		return err
	}
	defer sess.close(ctx)
	//nolint:gosec // G115: uids are non-negative 32-bit values
	uids := []uint64{uint64(msg.UID)}
	if dest != nil {
		if err := sess.client.UIDCopy(ctx, uids, dest.Name); err != nil {
			return mapOpsError(err)
		}
	}
	if err := sess.client.UIDStore(ctx, uids, []string{`\Deleted`}, nil); err != nil {
		return mapOpsError(err)
	}
	if err := sess.client.Expunge(ctx); err != nil {
		return mapOpsError(err)
	}
	return o.Store.DeleteMessage(ctx, src.ID, msg.ID)
}

// opsSession is one per-request connection: dialed, logged in, and with the
// mailbox SELECTed read-write. close logs out best-effort.
type opsSession struct {
	ops    *MessageOps
	client *imap.Client
}

func (o *MessageOps) openSession(ctx context.Context, a *Account, mb *Mailbox) (*opsSession, error) {
	imapPW, _, err := openPasswords(o.Secret, a.UserID, a.IMAPHost, a.IMAPUser, a.PasswordSealed)
	if err != nil {
		return nil, err // a sealed blob that does not open is a loud 500
	}
	dial := o.DialIMAP
	if dial == nil {
		dial = imap.Dial
	}
	client, err := dial(ctx, imap.DialOptions{Host: a.IMAPHost, Port: a.IMAPPort, SSLMode: a.IMAPSSLMode})
	if err != nil {
		return nil, fmt.Errorf("%w: dial: %w", ErrUpstream, err)
	}
	if err := client.Login(a.IMAPUser, imapPW); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: login: %w", ErrUpstream, err), client.Logout())
	}
	if _, err := client.Select(ctx, mb.Name); err != nil {
		return nil, errors.Join(mapOpsError(err), client.Logout())
	}
	return &opsSession{ops: o, client: client}, nil
}

// close logs out best-effort (ADR-0108 §1): a rude reply cannot un-run the
// op, so it only logs.
func (s *opsSession) close(ctx context.Context) {
	if err := s.client.Logout(); err != nil && s.ops.Logger != nil {
		s.ops.Logger.DebugContext(ctx, "mail: ops: logout failed", slog.String("error", err.Error()))
	}
}

// fetchRaw returns the whole RFC 822 message, guarding the size both before
// (the synced RFC822.SIZE) and after the fetch.
func (s *opsSession) fetchRaw(ctx context.Context, msg *Message) ([]byte, error) {
	if msg.Size > maxRawMessageSize {
		return nil, ErrMessageTooLarge
	}
	//nolint:gosec // G115: uids are non-negative 32-bit values
	raw, err := s.client.UIDFetchFull(ctx, uint64(msg.UID))
	if err != nil {
		return nil, mapOpsError(err)
	}
	if raw == nil {
		return nil, ErrMessageGone
	}
	if len(raw) > maxRawMessageSize {
		return nil, ErrMessageTooLarge
	}
	return raw, nil
}

// storeFlags runs the UID STORE and lands the local row update, returning
// the flag tokens after the delta.
func (s *opsSession) storeFlags(ctx context.Context, mb *Mailbox, msg *Message, add, remove []string) ([]string, error) {
	//nolint:gosec // G115: uids are non-negative 32-bit values
	if err := s.client.UIDStore(ctx, []uint64{uint64(msg.UID)}, add, remove); err != nil {
		return nil, mapOpsError(err)
	}
	tokens := applyFlagDelta(msg.Flags, add, remove)
	if err := s.ops.Store.SetMessageFlags(ctx, mb.ID, msg.UID, PackFlags(tokens)); err != nil {
		return nil, err
	}
	return tokens, nil
}

// mapOpsError classes one client failure: a tagged NO is the message or
// mailbox gone (404); anything transport/protocol is the upstream (502).
// Allowlist rejections (ErrInvalidFlag, ErrInjection) pass through
// unwrapped — they are caller input errors, not upstream faults.
func mapOpsError(err error) error {
	switch {
	case errors.Is(err, imap.ErrCommandRefused):
		return fmt.Errorf("%w: %w", ErrMessageGone, err)
	case errors.Is(err, imap.ErrInvalidFlag), errors.Is(err, imap.ErrInjection):
		return err
	default:
		return fmt.Errorf("%w: %w", ErrUpstream, err)
	}
}

// applyFlagDelta folds wire-form add/remove flags into the row's storage
// tokens, mirroring the wire order (UID STORE adds first, then removes, so
// a flag in both lists ends up removed). Tokens stay backslash-free;
// existing order survives and additions append.
func applyFlagDelta(current string, add, remove []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(add)+4)
	for _, tok := range strings.Fields(current) {
		if !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	for _, f := range add {
		tok := strings.TrimPrefix(f, "\\")
		if !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	drop := map[string]bool{}
	for _, f := range remove {
		drop[strings.TrimPrefix(f, "\\")] = true
	}
	if len(drop) > 0 {
		kept := out[:0]
		for _, tok := range out {
			if !drop[tok] {
				kept = append(kept, tok)
			}
		}
		out = kept
	}
	return out
}
