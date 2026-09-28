package adminworker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/munlucky/codex-account-pool/internal/adminapi"
	"github.com/munlucky/codex-account-pool/internal/process"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

type fakeLoginExecutor struct {
	t       *testing.T
	account string
	err     error
}

func (e fakeLoginExecutor) Run(_ context.Context, spec process.Command) error {
	e.t.Helper()
	if spec.Executable != "codex" {
		e.t.Fatalf("executable=%q", spec.Executable)
	}
	wantArgs := []string{"-c", `cli_auth_credentials_store="file"`, "login"}
	if !slices.Equal(spec.Args, wantArgs) {
		e.t.Fatalf("args=%q", spec.Args)
	}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		if !slices.Contains(spec.UnsetEnv, key) {
			e.t.Fatalf("missing unset env %s", key)
		}
	}
	if spec.Stdout != io.Discard || spec.Stderr != io.Discard {
		e.t.Fatal("Codex CLI output must be discarded")
	}
	if e.err != nil {
		return e.err
	}
	home := spec.SetEnv["CODEX_HOME"]
	if home == "" {
		e.t.Fatal("CODEX_HOME missing")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		e.t.Fatal(err)
	}
	writeWorkerAuth(e.t, filepath.Join(home, "auth.json"), e.account)
	return nil
}

type workerAPIFixture struct {
	mu       sync.Mutex
	leased   bool
	complete map[string]any
	job      leaseResponse
}

func (f *workerAPIFixture) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer worker-secret" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/admin/worker/diagnostics":
		w.WriteHeader(http.StatusNoContent)
	case "/admin/worker/lease":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.leased {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		f.leased = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.job)
	case "/admin/worker/jobs/" + f.job.JobID + "/complete":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.complete = body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "succeeded"})
	case "/admin/worker/jobs/" + f.job.JobID + "/heartbeat":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"state": adminapi.JobLoggingIn})
	case "/admin/worker/jobs/" + f.job.JobID + "/status":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.job.JobID, "state": adminapi.JobSucceeded})
	default:
		http.NotFound(w, r)
	}
}

func (f *workerAPIFixture) completed() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	for key, value := range f.complete {
		out[key] = value
	}
	return out
}

func TestWorkerRunsFixedCodexLoginAndReportsSuccess(t *testing.T) {
	store := profile.NewStore(t.TempDir())
	fixture := &workerAPIFixture{job: leaseResponse{
		JobID: "j_test", Provider: profile.ProviderCodex, ProfileID: "account-3",
		NewProfile: true, LeaseID: "l_test",
	}}
	server := httptest.NewServer(http.HandlerFunc(fixture.handler))
	defer server.Close()

	worker := &Worker{
		ServerURL: server.URL, WorkerKey: "worker-secret", Store: store,
		Executor: fakeLoginExecutor{t: t, account: "acct-three"}, CodexBin: "codex",
	}
	if err := worker.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	completed := fixture.completed()
	if completed["success"] != true || completed["error_code"] != "" || completed["lease_id"] != "l_test" {
		t.Fatalf("complete=%+v", completed)
	}
	data, err := os.ReadFile(store.CodexAuthPath("account-3"))
	if err != nil {
		t.Fatal(err)
	}
	if accountIDFromAuth(data) != "acct-three" {
		t.Fatal("new auth state was not retained")
	}
}

func TestWorkerRestoresPreviousAuthOnAccountMismatch(t *testing.T) {
	store := profile.NewStore(t.TempDir())
	if err := os.MkdirAll(store.CodexHome("account-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkerAuth(t, store.CodexAuthPath("account-1"), "acct-old")
	previous, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}

	fixture := &workerAPIFixture{job: leaseResponse{
		JobID: "j_mismatch", Provider: profile.ProviderCodex, ProfileID: "account-1",
		LeaseID: "l_mismatch",
	}}
	server := httptest.NewServer(http.HandlerFunc(fixture.handler))
	defer server.Close()

	worker := &Worker{
		ServerURL: server.URL, WorkerKey: "worker-secret", Store: store,
		Executor: fakeLoginExecutor{t: t, account: "acct-different"}, CodexBin: "codex",
	}
	if err := worker.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	completed := fixture.completed()
	if completed["success"] != false || completed["error_code"] != adminapi.ErrAccountMismatch {
		t.Fatalf("complete=%+v", completed)
	}
	current, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(previous) || accountIDFromAuth(current) != "acct-old" {
		t.Fatal("previous auth state was not restored")
	}
}

func TestWorkerRejectsNonLoopbackServer(t *testing.T) {
	worker := &Worker{
		ServerURL: "http://192.0.2.10:8317", WorkerKey: "worker-secret",
		Store: profile.NewStore(t.TempDir()), Executor: fakeLoginExecutor{t: t, account: "acct"},
	}
	if err := worker.validate(); err == nil {
		t.Fatal("expected non-loopback server URL rejection")
	}
}

func writeWorkerAuth(t *testing.T, path, account string) {
	t.Helper()
	document := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token":  "access-" + account,
			"refresh_token": "refresh-" + account,
			"account_id":    account,
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRestoresAuthWhenServerRejectsLateCompletion(t *testing.T) {
	store := profile.NewStore(t.TempDir())
	if err := os.MkdirAll(store.CodexHome("account-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkerAuth(t, store.CodexAuthPath("account-1"), "acct-old")
	previous, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}

	job := leaseResponse{
		JobID: "j_rejected", Provider: profile.ProviderCodex, ProfileID: "account-1",
		LeaseID: "l_rejected",
	}
	var mu sync.Mutex
	var leased bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer worker-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/admin/worker/diagnostics":
			w.WriteHeader(http.StatusNoContent)
		case "/admin/worker/lease":
			mu.Lock()
			defer mu.Unlock()
			if leased {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			leased = true
			_ = json.NewEncoder(w).Encode(job)
		case "/admin/worker/jobs/" + job.JobID + "/complete":
			http.Error(w, "inactive lease", http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	worker := &Worker{
		ServerURL: server.URL, WorkerKey: "worker-secret", Store: store,
		Executor: fakeLoginExecutor{t: t, account: "acct-old"}, CodexBin: "codex",
	}
	if err := worker.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(previous) {
		t.Fatal("rejected late completion did not restore previous auth state")
	}
}

func TestWorkerRestoresAuthWhenServerRejectsSuccessfulLoginState(t *testing.T) {
	store := profile.NewStore(t.TempDir())
	if err := os.MkdirAll(store.CodexHome("account-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkerAuth(t, store.CodexAuthPath("account-1"), "acct-old")
	previous, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}

	job := leaseResponse{
		JobID: "j_failed_state", Provider: profile.ProviderCodex, ProfileID: "account-1",
		LeaseID: "l_failed_state",
	}
	var mu sync.Mutex
	var leased bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer worker-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/admin/worker/diagnostics":
			w.WriteHeader(http.StatusNoContent)
		case "/admin/worker/lease":
			mu.Lock()
			defer mu.Unlock()
			if leased {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			leased = true
			_ = json.NewEncoder(w).Encode(job)
		case "/admin/worker/jobs/" + job.JobID + "/complete":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"state": adminapi.JobFailed})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	worker := &Worker{
		ServerURL: server.URL, WorkerKey: "worker-secret", Store: store,
		Executor: fakeLoginExecutor{t: t, account: "acct-old"}, CodexBin: "codex",
	}
	if err := worker.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(previous) {
		t.Fatal("server-side login rejection did not restore previous auth state")
	}
}

type cancelThenWriteExecutor struct {
	t       *testing.T
	started chan struct{}
	account string
}

func (e cancelThenWriteExecutor) Run(ctx context.Context, spec process.Command) error {
	e.t.Helper()
	close(e.started)
	<-ctx.Done()
	home := spec.SetEnv["CODEX_HOME"]
	if err := os.MkdirAll(home, 0o700); err != nil {
		e.t.Fatal(err)
	}
	writeWorkerAuth(e.t, filepath.Join(home, "auth.json"), e.account)
	return nil
}

func TestWorkerCancellationRestoresAuthEvenIfCodexReturnsNil(t *testing.T) {
	store := profile.NewStore(t.TempDir())
	if err := os.MkdirAll(store.CodexHome("account-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkerAuth(t, store.CodexAuthPath("account-1"), "acct-old")
	previous, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	worker := &Worker{
		ServerURL: "http://127.0.0.1:1", WorkerKey: "worker-secret", Store: store,
		Executor: cancelThenWriteExecutor{t: t, started: started, account: "acct-old"},
		CodexBin: "codex", Client: http.DefaultClient,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.runLogin(ctx, leaseResponse{
			JobID: "j_cancel", Provider: profile.ProviderCodex, ProfileID: "account-1", LeaseID: "l_cancel",
		})
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected cancellation error")
	}
	current, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(previous) {
		t.Fatal("canceled login retained replacement auth")
	}
}

func TestWorkerReconcilesFailedLoginBackupAfterRestart(t *testing.T) {
	store := profile.NewStore(t.TempDir())
	if err := os.MkdirAll(store.CodexHome("account-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkerAuth(t, store.CodexAuthPath("account-1"), "acct-old")
	previous, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLoginBackup(store, "account-1", "j_restart_failed", previous, true); err != nil {
		t.Fatal(err)
	}
	writeWorkerAuth(t, store.CodexAuthPath("account-1"), "acct-new")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer worker-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/admin/worker/diagnostics":
			w.WriteHeader(http.StatusNoContent)
		case "/admin/worker/lease":
			w.WriteHeader(http.StatusNoContent)
		case "/admin/worker/jobs/j_restart_failed/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "j_restart_failed", "state": adminapi.JobFailed})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	worker := &Worker{
		ServerURL: server.URL, WorkerKey: "worker-secret", Store: store,
		Executor: fakeLoginExecutor{t: t, account: "unused"}, CodexBin: "codex",
	}
	if err := worker.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(previous) || accountIDFromAuth(current) != "acct-old" {
		t.Fatal("failed persisted login did not restore previous auth")
	}
	if _, err := os.Stat(loginBackupPath(store, "account-1")); !os.IsNotExist(err) {
		t.Fatalf("login backup was not removed after restore: %v", err)
	}
}

func TestWorkerReconcilesSuccessfulLoginBackupWithoutRollback(t *testing.T) {
	store := profile.NewStore(t.TempDir())
	if err := os.MkdirAll(store.CodexHome("account-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkerAuth(t, store.CodexAuthPath("account-1"), "acct-old")
	previous, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLoginBackup(store, "account-1", "j_restart_success", previous, true); err != nil {
		t.Fatal(err)
	}
	writeWorkerAuth(t, store.CodexAuthPath("account-1"), "acct-new")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer worker-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/admin/worker/diagnostics":
			w.WriteHeader(http.StatusNoContent)
		case "/admin/worker/lease":
			w.WriteHeader(http.StatusNoContent)
		case "/admin/worker/jobs/j_restart_success/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "j_restart_success", "state": adminapi.JobSucceeded})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	worker := &Worker{
		ServerURL: server.URL, WorkerKey: "worker-secret", Store: store,
		Executor: fakeLoginExecutor{t: t, account: "unused"}, CodexBin: "codex",
	}
	if err := worker.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(store.CodexAuthPath("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if accountIDFromAuth(current) != "acct-new" {
		t.Fatal("successful persisted login was incorrectly rolled back")
	}
	if _, err := os.Stat(loginBackupPath(store, "account-1")); !os.IsNotExist(err) {
		t.Fatalf("login backup was not removed after success: %v", err)
	}
}
