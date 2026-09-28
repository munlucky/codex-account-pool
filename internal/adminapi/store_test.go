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
		if err := store.CompleteCheck(job.ID, CheckRecord{Status: StatusConnected, CheckedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.state.Jobs) != maxRetainedTerminalJobs {
		t.Fatalf("jobs=%d", len(store.state.Jobs))
	}
}

func TestLoginChallengeAndFinalizationTransitions(t *testing.T) {
	store, err := OpenStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(JobTypeLogin, "codex", "account-1", false)
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.SetLoginChallenge(job.ID, "https://auth.openai.com/codex/device", "ABCD-1234")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobLoggingIn || job.UserCode == "" {
		t.Fatalf("challenge=%+v", job)
	}
	job, err = store.BeginLoginFinalization(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobFinalizing || job.UserCode != "" {
		t.Fatalf("finalizing=%+v", job)
	}
	job, err = store.FinishLogin(job.ID, true, "")
	if err != nil || job.State != JobSucceeded {
		t.Fatalf("finished=%+v err=%v", job, err)
	}
}

func TestRestartFailsContainerOwnedLogin(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(JobTypeLogin, "codex", "account-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetLoginChallenge(job.ID, "https://auth.openai.com/codex/device", "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Job(job.ID)
	if !ok || got.State != JobFailed || got.ErrorCode != ErrServiceRestarted || got.UserCode != "" {
		t.Fatalf("job=%+v ok=%v", got, ok)
	}
}
