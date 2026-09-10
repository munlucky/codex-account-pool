package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	processpkg "github.com/munlucky/codex-account-pool/internal/process"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

type fakeExecutor struct {
	commands []processpkg.Command
	err      error
}

func (f *fakeExecutor) Run(_ context.Context, cmd processpkg.Command) error {
	f.commands = append(f.commands, cmd)
	return f.err
}

func newTestApp(t *testing.T) (*App, *fakeExecutor, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	exec := &fakeExecutor{}
	a := New(profile.NewStore(t.TempDir()), exec, out, out, "test")
	a.CodexBin = "fake-codex"
	return a, exec, out
}

func TestCodexAuthFlowUsesIsolatedFileBackedCodexHome(t *testing.T) {
	a, exec, _ := newTestApp(t)
	ctx := context.Background()
	if err := a.Execute(ctx, []string{"auth", "add", "codex", "account-1"}); err != nil {
		t.Fatal(err)
	}
	if len(exec.commands) != 1 {
		t.Fatalf("commands=%d", len(exec.commands))
	}
	login := exec.commands[0]
	if login.Executable != "fake-codex" || strings.Join(login.Args, " ") != `-c cli_auth_credentials_store="file" login` {
		t.Fatalf("unexpected login command: %+v", login)
	}
	if login.SetEnv["CODEX_HOME"] != a.Store.CodexHome("account-1") {
		t.Fatalf("CODEX_HOME=%q", login.SetEnv["CODEX_HOME"])
	}
	unset := strings.Join(login.UnsetEnv, ",")
	for _, secretEnv := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		if !strings.Contains(unset, secretEnv) {
			t.Fatalf("%s was not stripped: %v", secretEnv, login.UnsetEnv)
		}
	}

	if err := a.Execute(ctx, []string{"auth", "status", "codex"}); err != nil {
		t.Fatal(err)
	}
	status := exec.commands[1]
	if strings.Join(status.Args, " ") != `-c cli_auth_credentials_store="file" login status` {
		t.Fatalf("unexpected status command: %+v", status)
	}

	if err := a.Execute(ctx, []string{"run", "codex", "--", "--model", "gpt-test"}); err != nil {
		t.Fatal(err)
	}
	run := exec.commands[2]
	if strings.Join(run.Args, " ") != `-c cli_auth_credentials_store="file" --model gpt-test` || run.SetEnv["CODEX_HOME"] != a.Store.CodexHome("account-1") {
		t.Fatalf("unexpected run command: %+v", run)
	}
}

func TestCodexProfilesSwitchManually(t *testing.T) {
	a, exec, out := newTestApp(t)
	ctx := context.Background()
	for _, id := range []string{"one", "two"} {
		if err := a.Execute(ctx, []string{"auth", "add", "codex", id}); err != nil {
			t.Fatal(err)
		}
	}
	if exec.commands[0].SetEnv["CODEX_HOME"] == exec.commands[1].SetEnv["CODEX_HOME"] {
		t.Fatal("codex profiles must have independent CODEX_HOME values")
	}
	out.Reset()
	if err := a.Execute(ctx, []string{"auth", "use", "codex", "two"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "usage-limit response may fail over automatically") {
		t.Fatalf("quota failover notice missing: %s", out.String())
	}
	if err := a.Execute(ctx, []string{"run", "codex"}); err != nil {
		t.Fatal(err)
	}
	if got := exec.commands[2].SetEnv["CODEX_HOME"]; got != a.Store.CodexHome("two") {
		t.Fatalf("run used CODEX_HOME=%q", got)
	}
}

func TestCodexExistingProfileCanLoginAgain(t *testing.T) {
	a, exec, _ := newTestApp(t)
	ctx := context.Background()
	if err := a.Execute(ctx, []string{"auth", "add", "codex", "one"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Execute(ctx, []string{"auth", "login", "codex", "one"}); err != nil {
		t.Fatal(err)
	}
	if len(exec.commands) != 2 || strings.Join(exec.commands[1].Args, " ") != `-c cli_auth_credentials_store="file" login` {
		t.Fatalf("unexpected re-login command: %+v", exec.commands)
	}
	if err := a.Execute(ctx, []string{"auth", "login", "codex", "missing"}); err == nil {
		t.Fatal("expected missing profile error")
	}
}

func TestAuthListContainsNoCredentialValuesOrFields(t *testing.T) {
	a, _, out := newTestApp(t)
	if err := a.Execute(context.Background(), []string{"auth", "add", "codex", "one"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.Execute(context.Background(), []string{"auth", "list"}); err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(out.String())
	if !strings.Contains(text, "gpt codex router auth broker") {
		t.Fatalf("auth ownership missing: %s", text)
	}
	for _, forbidden := range []string{"access_token", "refresh_token", "bearer ", "client_secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("credential field %q leaked into list output: %s", forbidden, text)
		}
	}
}

func TestFailedCodexLoginDoesNotRegisterProfile(t *testing.T) {
	a, exec, _ := newTestApp(t)
	exec.err = errors.New("fake login failure")
	if err := a.Execute(context.Background(), []string{"auth", "add", "codex", "broken"}); err == nil {
		t.Fatal("expected login failure")
	}
	registry, err := a.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Find(profile.ProviderCodex, "broken"); ok {
		t.Fatal("failed login must not register the profile")
	}
}

func TestServeAddressIsLoopbackOnlyByDefault(t *testing.T) {
	t.Setenv("GPT_CODEX_ROUTER_CONTAINER", "")
	for _, address := range []string{"127.0.0.1:8317", "localhost:8317", "[::1]:8317"} {
		if err := requireSafeListenAddress(address); err != nil {
			t.Fatalf("%s rejected: %v", address, err)
		}
	}
	for _, address := range []string{":8317", "0.0.0.0:8317", "[::]:8317", "192.168.1.20:8317", "bad"} {
		if err := requireSafeListenAddress(address); err == nil {
			t.Fatalf("expected %s to be rejected", address)
		}
	}
}

func TestContainerModeAllowsOnlyUnspecifiedContainerListen(t *testing.T) {
	t.Setenv("GPT_CODEX_ROUTER_CONTAINER", "1")
	for _, address := range []string{"0.0.0.0:8317", "[::]:8317", "127.0.0.1:8317"} {
		if err := requireSafeListenAddress(address); err != nil {
			t.Fatalf("%s rejected in container mode: %v", address, err)
		}
	}
	for _, address := range []string{"192.168.1.20:8317", "10.0.0.10:8317"} {
		if err := requireSafeListenAddress(address); err == nil {
			t.Fatalf("container mode must not allow specific LAN address %s", address)
		}
	}
}

func TestServerHealthEndpointIsLocalAndDoesNotReachProxy(t *testing.T) {
	proxyCalls := 0
	handler := serverHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls++
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://localhost/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" || proxyCalls != 0 {
		t.Fatalf("health status=%d body=%q proxyCalls=%d", rec.Code, rec.Body.String(), proxyCalls)
	}

	req = httptest.NewRequest(http.MethodPost, "http://localhost/backend-api/codex/responses", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || proxyCalls != 1 {
		t.Fatalf("proxy status=%d proxyCalls=%d", rec.Code, proxyCalls)
	}
}

func TestServeArgsDefaultTo8317(t *testing.T) {
	got, err := parseServeArgs(nil)
	if err != nil || got != "127.0.0.1:8317" {
		t.Fatalf("listen=%q err=%v", got, err)
	}
	got, err = parseServeArgs([]string{"--listen", "localhost:9000"})
	if err != nil || got != "localhost:9000" {
		t.Fatalf("listen=%q err=%v", got, err)
	}
}

func TestHelpIncludesQuotaAwareDesktopRouting(t *testing.T) {
	a, _, out := newTestApp(t)
	if err := a.Execute(context.Background(), []string{"help"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, required := range []string{
		`chatgpt_base_url = "http://127.0.0.1:8317/backend-api"`,
		`openai_base_url = "http://127.0.0.1:8317/backend-api/codex"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("help missing %q: %s", required, text)
		}
	}
}

func TestDesktopSurfaceCommandWasRemoved(t *testing.T) {
	a, _, _ := newTestApp(t)
	if err := a.Execute(context.Background(), []string{"doctor", "codex-desktop"}); err == nil {
		t.Fatal("legacy desktop doctor must no longer exist")
	}
	if err := a.Execute(context.Background(), []string{"run", "codex", "--surface", "desktop"}); err == nil || !strings.Contains(err.Error(), "--surface was removed") {
		t.Fatalf("legacy desktop run surface was not rejected: %v", err)
	}
}

func TestCodexAuthPathIsNotDesktopStatePath(t *testing.T) {
	a, _, _ := newTestApp(t)
	want := filepath.Join(a.Store.CodexHome("one"), "auth.json")
	if got := a.Store.CodexAuthPath("one"); got != want {
		t.Fatalf("auth path=%q want=%q", got, want)
	}
}
