package mail

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

// fakeAccountSyncer records the accounts it was asked to sync and fails for
// one configured user — the job test's sync seam.
type fakeAccountSyncer struct {
	mu      sync.Mutex
	seen    []int64
	failFor string
}

func (f *fakeAccountSyncer) SyncAccount(_ context.Context, a *Account) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, a.ID)
	if a.UserID == f.failFor {
		return 0, errors.New("boom")
	}
	return 0, nil
}

func TestSyncJobFansOutAndIsolatesFailures(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	for _, uid := range []string{"alice", "bob", "carol"} {
		if err := store.Create(ctx, testAccount(uid)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	syncer := &fakeAccountSyncer{failFor: "bob"}
	job := NewSyncJob(syncer, store, slog.New(slog.DiscardHandler))
	if job.Name() != jobs.JobMailSync {
		t.Fatalf("name = %q", job.Name())
	}
	// bob's failure must not stop alice and carol, and must not fail Run.
	if err := job.Run(ctx, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if len(syncer.seen) != 3 || syncer.seen[0] != all[0].ID || syncer.seen[1] != all[1].ID || syncer.seen[2] != all[2].ID {
		t.Errorf("seen = %v", syncer.seen)
	}
}

func TestSyncJobListFailureRetries(t *testing.T) {
	job := NewSyncJob(&fakeAccountSyncer{}, failingLister{}, slog.New(slog.DiscardHandler))
	if err := job.Run(context.Background(), nil); err == nil {
		t.Fatal("Run with a broken account listing succeeded")
	}
}

type failingLister struct{}

func (failingLister) ListAll(context.Context) ([]Account, error) {
	return nil, errors.New("db down")
}
