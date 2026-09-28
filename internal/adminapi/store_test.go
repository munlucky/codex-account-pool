package adminapi

import (
	"fmt"
	"testing"
	"time"
)

func TestStateStoreBoundsTerminalJobHistory(t *testing.T) {
	store, err := OpenStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRetainedTerminalJobs+40; i++ {
		job, err := store.CreateJob(JobTypeCheck, "codex", fmt.Sprintf("account-%d", i), false)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteCheck(job.ID, CheckRecord{
			Status: StatusConnected, CheckedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	terminal := 0
	for _, job := range store.state.Jobs {
		switch job.State {
		case JobSucceeded, JobFailed, JobCanceled:
			terminal++
		}
	}
	if terminal != maxRetainedTerminalJobs {
		t.Fatalf("terminal jobs=%d want=%d", terminal, maxRetainedTerminalJobs)
	}
}

func TestExpiredLoginLeaseCannotBeRevivedByHeartbeat(t *testing.T) {
	store, err := OpenStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0).UTC()
	store.now = func() time.Time { return now }

	job, err := store.CreateJob(JobTypeLogin, "codex", "account-1", false)
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.LeaseLogin(20 * time.Second)
	if err != nil || !ok || leased.ID != job.ID {
		t.Fatalf("lease=%+v ok=%v err=%v", leased, ok, err)
	}
	now = now.Add(21 * time.Second)

	got, err := store.Heartbeat(leased.ID, leased.LeaseID, 20*time.Second)
	if err == nil {
		t.Fatal("expired lease heartbeat must fail")
	}
	if got.State != JobFailed || got.ErrorCode != ErrWorkerRestarted || got.LeaseID != "" || !got.LeaseUntil.IsZero() {
		t.Fatalf("expired heartbeat state=%+v", got)
	}
}

func TestExpiredLoginLeaseCannotBeginCompletion(t *testing.T) {
	store, err := OpenStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0).UTC()
	store.now = func() time.Time { return now }

	job, err := store.CreateJob(JobTypeLogin, "codex", "account-1", false)
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.LeaseLogin(20 * time.Second)
	if err != nil || !ok || leased.ID != job.ID {
		t.Fatalf("lease=%+v ok=%v err=%v", leased, ok, err)
	}
	now = now.Add(21 * time.Second)

	got, err := store.BeginLoginCompletion(leased.ID, leased.LeaseID)
	if err == nil {
		t.Fatal("expired lease completion must fail")
	}
	if got.State != JobFailed || got.ErrorCode != ErrWorkerRestarted || got.LeaseID != "" || !got.LeaseUntil.IsZero() {
		t.Fatalf("expired completion state=%+v", got)
	}
}
