package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/gateway"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

type fixedChecker struct {
	result CheckResult
}

func (c fixedChecker) Check(context.Context, string) CheckResult { return c.result }

func TestExpiredAccessTokenStaysUnverifiedUntilCheck(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	writeAdminTestAuth(t, store, "one", adminTestJWT(t, now.Add(-time.Minute)), "refresh-one", "acct-one")

	broker := authbroker.New(store)
	broker.Now = func() time.Time { return now }
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, broker, state, nil)
	service.Now = func() time.Time { return now }

	list, err := service.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Profiles) != 1 || list.Profiles[0].Status != StatusAccessExpiredUnverified {
		t.Fatalf("profiles=%+v", list.Profiles)
	}

	if err := state.RecordCheck(profile.ProviderCodex, "one", CheckRecord{
		Status: StatusReauthRequired, ErrorCode: ErrReauthRequired, CheckedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	list, err = service.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if list.Profiles[0].Status != StatusReauthRequired || list.Profiles[0].LastCheckedAt == nil {
		t.Fatalf("checked profile=%+v", list.Profiles[0])
	}
}

func TestNewLoginRegistrationPreservesExistingActiveProfile(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}

	broker := authbroker.New(store)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	service := NewService(store, broker, state, fixedChecker{result: CheckResult{
		Status: StatusConnected, CheckedAt: now,
	}})
	job, err := service.StartLogin(profile.ProviderCodex, "two")
	if err != nil {
		t.Fatal(err)
	}
	if !job.NewProfile {
		t.Fatal("new profile was not marked as new")
	}
	leased, ok, err := service.WorkerLease()
	if err != nil || !ok {
		t.Fatalf("lease=%+v ok=%v err=%v", leased, ok, err)
	}
	writeAdminTestAuth(t, store, "two", "access-two", "refresh-two", "acct-two")

	finished, err := service.WorkerFinish(leased.ID, leased.LeaseID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != JobLoggingIn {
		t.Fatalf("accepted completion=%+v", finished)
	}
	finished = waitForTerminalJob(t, service, leased.ID)
	if finished.State != JobSucceeded {
		t.Fatalf("finished=%+v", finished)
	}
	registry, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	active, ok := registry.ActiveProfile(profile.ProviderCodex)
	if !ok || active.ID != "one" {
		t.Fatalf("active changed: %+v ok=%v", active, ok)
	}
	if _, ok := registry.Find(profile.ProviderCodex, "two"); !ok {
		t.Fatal("new profile was not registered")
	}
}

func TestRestartMarksInFlightLoginFailed(t *testing.T) {
	root := t.TempDir()
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	job, err := state.CreateJob(JobTypeLogin, profile.ProviderCodex, "one", false)
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := state.LeaseLogin(time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease=%+v ok=%v err=%v", leased, ok, err)
	}
	if leased.State != JobLoggingIn {
		t.Fatalf("state=%s", leased.State)
	}

	reopened, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Job(job.ID)
	if !ok || got.State != JobFailed || got.ErrorCode != ErrServiceRestarted {
		t.Fatalf("job after restart=%+v ok=%v", got, ok)
	}
}

func writeAdminTestAuth(t *testing.T, store *profile.Store, id, access, refresh, account string) {
	t.Helper()
	if err := os.MkdirAll(store.CodexHome(id), 0o700); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"account_id":    account,
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.CodexAuthPath(id), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func adminTestJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("e30.%s.sig", base64.RawURLEncoding.EncodeToString(payload))
}

func TestCanceledLoginRejectsLateWorkerCompletionWithoutRegistration(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}

	broker := authbroker.New(store)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, broker, state, fixedChecker{result: CheckResult{
		Status: StatusConnected, CheckedAt: time.Now().UTC(),
	}})
	job, err := service.StartLogin(profile.ProviderCodex, "two")
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := service.WorkerLease()
	if err != nil || !ok {
		t.Fatalf("lease=%+v ok=%v err=%v", leased, ok, err)
	}
	if _, err := service.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	writeAdminTestAuth(t, store, "two", "access-two", "refresh-two", "acct-two")

	if _, err := service.WorkerFinish(leased.ID, leased.LeaseID, true, ""); err == nil {
		t.Fatal("late worker completion after cancellation must be rejected")
	}
	registry, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := registry.Find(profile.ProviderCodex, "two"); exists {
		t.Fatal("canceled login registered a new profile")
	}
	got, ok := state.Job(job.ID)
	if !ok || got.State != JobCanceled || got.ErrorCode != ErrCanceled {
		t.Fatalf("canceled job=%+v ok=%v", got, ok)
	}
}

func waitForTerminalJob(t *testing.T, service *Service, id string) PublicJob {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := service.Job(id)
		if !ok {
			t.Fatalf("job %s disappeared", id)
		}
		if job.State == JobSucceeded || job.State == JobFailed || job.State == JobCanceled {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := service.Job(id)
	t.Fatalf("job did not reach terminal state: %+v", job)
	return PublicJob{}
}

func TestCodexCheckerPreservesTemporaryRefreshFailure(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(2_000_000_000, 0)
	writeAdminTestAuth(t, store, "one", adminTestJWT(t, now.Add(time.Hour)), "refresh-one", "acct-one")

	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer refreshServer.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	broker := authbroker.New(store)
	broker.Now = func() time.Time { return now }
	broker.RefreshURL = refreshServer.URL
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := gateway.New(broker, upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	checker := &CodexChecker{
		Broker: broker, Gateway: handler, ClientVersion: "0.1.0",
		Now: func() time.Time { return now },
	}
	result := checker.Check(context.Background(), "one")
	if result.Status != StatusTemporarilyUnavailable || result.ErrorCode != ErrTemporary {
		t.Fatalf("result=%+v", result)
	}
}

func TestCodexCheckerMissingAuthIsNotLoggedIn(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}

	broker := authbroker.New(store)
	upstreamURL, _ := url.Parse("http://127.0.0.1:1")
	handler, err := gateway.New(broker, upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	checker := &CodexChecker{Broker: broker, Gateway: handler, ClientVersion: "0.1.0"}
	result := checker.Check(context.Background(), "one")
	if result.Status != StatusNotLoggedIn || result.ErrorCode != ErrNotLoggedIn {
		t.Fatalf("result=%+v", result)
	}
}
