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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/codexlogin"
	"github.com/munlucky/codex-account-pool/internal/gateway"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

type fixedChecker struct{ result CheckResult }

func (c fixedChecker) Check(context.Context, string) CheckResult { return c.result }

type loginRunnerFunc func(context.Context, string, func(codexlogin.Challenge) error) error

func (f loginRunnerFunc) Login(ctx context.Context, home string, cb func(codexlogin.Challenge) error) error {
	return f(ctx, home, cb)
}

func TestExpiredAccessTokenStaysUnverifiedUntilCheck(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, _ := store.Load()
	_ = registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"})
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
}

func TestContainerLoginRegistersNewProfileAndPreservesExistingActive(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, _ := store.Load()
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
	service := NewService(store, broker, state, fixedChecker{result: CheckResult{Status: StatusConnected, CheckedAt: now}})
	service.LoginRunner = loginRunnerFunc(func(ctx context.Context, home string, cb func(codexlogin.Challenge) error) error {
		if err := cb(codexlogin.Challenge{LoginID: "login-1", VerificationURL: "https://auth.openai.com/codex/device", UserCode: "ABCD-1234"}); err != nil {
			return err
		}
		writeAuthAtHome(t, home, "access-two", "refresh-two", "acct-two")
		return nil
	})
	job, err := service.StartLogin(profile.ProviderCodex, "two")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForTerminalJob(t, service, job.ID)
	if finished.State != JobSucceeded {
		t.Fatalf("finished=%+v", finished)
	}
	registry, _ = store.Load()
	active, ok := registry.ActiveProfile(profile.ProviderCodex)
	if !ok || active.ID != "one" {
		t.Fatalf("active=%+v ok=%v", active, ok)
	}
	if _, ok := registry.Find(profile.ProviderCodex, "two"); !ok {
		t.Fatal("new profile not registered")
	}
}

func TestReloginAccountMismatchRestoresPreviousAuth(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, _ := store.Load()
	_ = registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"})
	_ = store.Save(registry)
	writeAdminTestAuth(t, store, "one", "old-access", "old-refresh", "acct-old")
	before, _ := os.ReadFile(store.CodexAuthPath("one"))
	state, _ := OpenStateStore(root)
	service := NewService(store, authbroker.New(store), state, fixedChecker{result: CheckResult{Status: StatusConnected, CheckedAt: time.Now().UTC()}})
	service.LoginRunner = loginRunnerFunc(func(ctx context.Context, home string, cb func(codexlogin.Challenge) error) error {
		if err := cb(codexlogin.Challenge{LoginID: "login-2", VerificationURL: "https://auth.openai.com/codex/device", UserCode: "WXYZ-5678"}); err != nil {
			return err
		}
		writeAuthAtHome(t, home, "new-access", "new-refresh", "acct-other")
		return nil
	})
	job, err := service.StartLogin(profile.ProviderCodex, "one")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForTerminalJob(t, service, job.ID)
	if finished.State != JobFailed || finished.ErrorCode != ErrAccountMismatch {
		t.Fatalf("finished=%+v", finished)
	}
	after, _ := os.ReadFile(store.CodexAuthPath("one"))
	if string(after) != string(before) {
		t.Fatal("previous auth was not restored")
	}
}

func TestCancelDeviceLoginDoesNotRegisterProfile(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	state, _ := OpenStateStore(root)
	service := NewService(store, authbroker.New(store), state, nil)
	service.LoginRunner = loginRunnerFunc(func(ctx context.Context, home string, cb func(codexlogin.Challenge) error) error {
		if err := cb(codexlogin.Challenge{LoginID: "login-3", VerificationURL: "https://auth.openai.com/codex/device", UserCode: "CANCEL-1"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	job, err := service.StartLogin(profile.ProviderCodex, "two")
	if err != nil {
		t.Fatal(err)
	}
	waitForChallenge(t, service, job.ID)
	canceled, err := service.CancelJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != JobCanceled {
		t.Fatalf("canceled=%+v", canceled)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(loginBackupPath(store, "two")); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	registry, _ := store.Load()
	if _, exists := registry.Find(profile.ProviderCodex, "two"); exists {
		t.Fatal("canceled new login registered profile")
	}
}

func TestReconcileRestoresAuthAfterContainerRestart(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, _ := store.Load()
	_ = registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"})
	_ = store.Save(registry)
	writeAdminTestAuth(t, store, "one", "old-access", "old-refresh", "acct-old")
	previous, _ := os.ReadFile(store.CodexAuthPath("one"))
	state, _ := OpenStateStore(root)
	job, err := state.CreateJob(JobTypeLogin, profile.ProviderCodex, "one", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLoginBackup(store, "one", job.ID, previous, true); err != nil {
		t.Fatal(err)
	}
	writeAdminTestAuth(t, store, "one", "new-access", "new-refresh", "acct-old")
	reopened, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, authbroker.New(store), reopened, nil)
	if err := service.ReconcileLoginBackups(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(store.CodexAuthPath("one"))
	if string(after) != string(previous) {
		t.Fatal("restart reconciliation did not restore auth")
	}
}

func writeAdminTestAuth(t *testing.T, store *profile.Store, id, access, refresh, account string) {
	t.Helper()
	if err := os.MkdirAll(store.CodexHome(id), 0o700); err != nil {
		t.Fatal(err)
	}
	writeAuthAtHome(t, store.CodexHome(id), access, refresh, account)
}

func writeAuthAtHome(t *testing.T, home, access, refresh, account string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"access_token": access, "refresh_token": refresh, "account_id": account}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
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

func waitForChallenge(t *testing.T, service *Service, id string) PublicJob {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := service.Job(id)
		if ok && job.UserCode != "" {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not expose challenge", id)
	return PublicJob{}
}

func waitForTerminalJob(t *testing.T, service *Service, id string) PublicJob {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := service.Job(id)
		if !ok {
			t.Fatalf("job disappeared")
		}
		if job.State == JobSucceeded || job.State == JobFailed || job.State == JobCanceled {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := service.Job(id)
	t.Fatalf("job did not finish: %+v", job)
	return PublicJob{}
}

func TestCodexCheckerPreservesTemporaryRefreshFailure(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, _ := store.Load()
	_ = registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"})
	_ = store.Save(registry)
	now := time.Unix(2_000_000_000, 0)
	writeAdminTestAuth(t, store, "one", adminTestJWT(t, now.Add(time.Hour)), "refresh-one", "acct-one")
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer refreshServer.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unauthorized", http.StatusUnauthorized) }))
	defer upstream.Close()
	broker := authbroker.New(store)
	broker.Now = func() time.Time { return now }
	broker.RefreshURL = refreshServer.URL
	upstreamURL, _ := url.Parse(upstream.URL)
	handler, err := gateway.New(broker, upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	checker := &CodexChecker{Broker: broker, Gateway: handler, ClientVersion: "0.1.0", Now: func() time.Time { return now }}
	result := checker.Check(context.Background(), "one")
	if result.Status != StatusTemporarilyUnavailable || result.ErrorCode != ErrTemporary {
		t.Fatalf("result=%+v", result)
	}
}

func TestCodexCheckerMissingAuthIsNotLoggedIn(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, _ := store.Load()
	_ = registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"})
	_ = store.Save(registry)
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

func TestOnlyOneDeviceLoginRunsAtATime(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, authbroker.New(store), state, nil)
	service.LoginRunner = loginRunnerFunc(func(ctx context.Context, home string, cb func(codexlogin.Challenge) error) error {
		if err := cb(codexlogin.Challenge{LoginID: "login-one", VerificationURL: "https://auth.openai.com/codex/device", UserCode: "ONE-1111"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})

	first, err := service.StartLogin(profile.ProviderCodex, "one")
	if err != nil {
		t.Fatal(err)
	}
	waitForChallenge(t, service, first.ID)
	if _, err := service.StartLogin(profile.ProviderCodex, "two"); err == nil || !strings.Contains(err.Error(), "another login operation") {
		t.Fatalf("expected global login serialization, got %v", err)
	}
	if _, err := service.CancelJob(first.ID); err != nil {
		t.Fatal(err)
	}
	_ = waitForTerminalJob(t, service, first.ID)
}

func TestProfileListExposesBoundedActiveLoginChallenge(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, authbroker.New(store), state, nil)
	service.LoginRunner = loginRunnerFunc(func(ctx context.Context, home string, cb func(codexlogin.Challenge) error) error {
		if err := cb(codexlogin.Challenge{LoginID: "login-refresh", VerificationURL: "https://auth.openai.com/codex/device", UserCode: "REFRESH-1"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})

	job, err := service.StartLogin(profile.ProviderCodex, "new-profile")
	if err != nil {
		t.Fatal(err)
	}
	waitForChallenge(t, service, job.ID)
	list, err := service.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.ActiveJobs) != 1 || list.ActiveJobs[0].ID != job.ID || list.ActiveJobs[0].UserCode != "REFRESH-1" {
		t.Fatalf("active jobs=%+v", list.ActiveJobs)
	}
	if _, err := service.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	_ = waitForTerminalJob(t, service, job.ID)
}

func TestNewProfileFailedVerificationRollsBackRegistrationAndAuth(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	registry, _ := store.Load()
	if err := registry.Add(profile.Profile{ID: "one", Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, authbroker.New(store), state, fixedChecker{result: CheckResult{
		Status: StatusTemporarilyUnavailable, ErrorCode: ErrTemporary, CheckedAt: time.Now().UTC(),
	}})
	service.LoginRunner = loginRunnerFunc(func(ctx context.Context, home string, cb func(codexlogin.Challenge) error) error {
		if err := cb(codexlogin.Challenge{LoginID: "login-fail-check", VerificationURL: "https://auth.openai.com/codex/device", UserCode: "FAIL-1234"}); err != nil {
			return err
		}
		writeAuthAtHome(t, home, "access-two", "refresh-two", "acct-two")
		return nil
	})

	job, err := service.StartLogin(profile.ProviderCodex, "two")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForTerminalJob(t, service, job.ID)
	if finished.State != JobFailed || finished.ErrorCode != ErrTemporary {
		t.Fatalf("finished=%+v", finished)
	}
	registry, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := registry.Find(profile.ProviderCodex, "two"); exists {
		t.Fatal("failed new-profile verification left registry entry")
	}
	if _, err := os.Stat(store.CodexAuthPath("two")); !os.IsNotExist(err) {
		t.Fatalf("failed new-profile verification left auth file: %v", err)
	}
	active, ok := registry.ActiveProfile(profile.ProviderCodex)
	if !ok || active.ID != "one" {
		t.Fatalf("active=%+v ok=%v", active, ok)
	}
}
