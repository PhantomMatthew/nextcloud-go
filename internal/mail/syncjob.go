package mail

import (
	"context"
	"log/slog"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

// syncTimeout bounds one account's sync inside the job (ADR-0108 §5).
const syncTimeout = 2 * time.Minute

// AccountLister is the job's account source: mail.sync fans out over ALL
// users' accounts, unlike the per-user API paths.
type AccountLister interface {
	ListAll(ctx context.Context) ([]Account, error)
}

// AccountSyncer is the job's sync seam (a fake drives the fan-out tests).
type AccountSyncer interface {
	SyncAccount(ctx context.Context, a *Account) (int, error)
}

// SyncJob is the periodic mail.sync job (ADR-0108 §5): list every account,
// sync each sequentially, and isolate failures — one broken account is
// logged, never fatal, so the rest still sync and the row completes.
type SyncJob struct {
	Syncer   AccountSyncer
	Accounts AccountLister
	Logger   *slog.Logger
}

// NewSyncJob wires the mail.sync job over the production Syncer and store.
func NewSyncJob(syncer AccountSyncer, accounts AccountLister, logger *slog.Logger) *SyncJob {
	return &SyncJob{Syncer: syncer, Accounts: accounts, Logger: logger}
}

func (j *SyncJob) Name() string { return jobs.JobMailSync }

func (j *SyncJob) Run(ctx context.Context, _ []byte) error {
	accounts, err := j.Accounts.ListAll(ctx)
	if err != nil {
		return err // a listing failure retries the whole job
	}
	for i := range accounts {
		actx, cancel := context.WithTimeout(ctx, syncTimeout)
		_, err := j.Syncer.SyncAccount(actx, &accounts[i])
		cancel()
		if err != nil && j.Logger != nil {
			j.Logger.WarnContext(ctx, "mail: sync job: account failed",
				slog.String("user", accounts[i].UserID), slog.Int64("account", accounts[i].ID), slog.String("error", err.Error()))
		}
	}
	return nil
}
