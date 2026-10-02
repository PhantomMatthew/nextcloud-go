package search

import (
	"context"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail"
)

// MailProvider searches the caller's synced mail (M6, ADR-0108): subjects
// and senders across every account and mailbox the user owns. It is
// registered only when the Mail app is enabled, so a nil store simply
// yields no hits.
type MailProvider struct {
	Mail mail.Store
}

// NewMailProvider returns the mail unified-search provider.
func NewMailProvider(store mail.Store) *MailProvider {
	return &MailProvider{Mail: store}
}

func (p *MailProvider) ID() string   { return "mail" }
func (p *MailProvider) Name() string { return "Mail" }

func (p *MailProvider) Search(ctx context.Context, uid, term string, limit int) ([]Hit, error) {
	if p == nil || p.Mail == nil {
		return nil, nil
	}
	if strings.TrimSpace(term) == "" {
		return nil, nil
	}
	found, err := p.Mail.SearchMessages(ctx, uid, term, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(found))
	for i := range found {
		h := &found[i]
		out = append(out, Hit{
			Title:       h.Subject,
			Subline:     h.FromAddr,
			ResourceURL: messagePath(h.AccountID, h.MailboxID, h.ID),
		})
	}
	return out, nil
}

// messagePath renders the message's JSON API path — the handler prefixes
// the request base, mirroring the files provider's /index.php/f/{id} link.
func messagePath(accountID, mailboxID, messageID int64) string {
	return mail.AccountsPrefix + "/" + strconv.FormatInt(accountID, 10) +
		"/mailboxes/" + strconv.FormatInt(mailboxID, 10) +
		"/messages/" + strconv.FormatInt(messageID, 10)
}
