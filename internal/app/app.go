package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/gateway"
	"github.com/munlucky/codex-account-pool/internal/observability"
	"github.com/munlucky/codex-account-pool/internal/process"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

var ErrUsage = errors.New("invalid command usage")

const defaultListenAddress = "127.0.0.1:8317"

type App struct {
	Store    *profile.Store
	Executor process.Executor
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	CodexBin string
	Version  string
	Commit   string
}

func New(store *profile.Store, executor process.Executor, out, errOut io.Writer, version string) *App {
	codexBin := os.Getenv("GPT_CODEX_ROUTER_CODEX_BIN")
	if codexBin == "" {
		codexBin = "codex"
	}
	return &App{
		Store: store, Executor: executor, In: os.Stdin, Out: out, Err: errOut,
		CodexBin: codexBin, Version: version, Commit: "unknown",
	}
}

func (a *App) Execute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		a.printUsage()
		return ErrUsage
	}
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Fprintln(a.Out, a.Version)
		return nil
	case "auth":
		return a.executeAuth(ctx, args[1:])
	case "run":
		return a.executeRun(ctx, args[1:])
	case "serve":
		return a.executeServe(ctx, args[1:])
	case "report":
		return a.executeReport(args[1:])
	case "help", "--help", "-h":
		a.printUsage()
		return nil
	default:
		a.printUsage()
		return fmt.Errorf("%w: unknown command %q", ErrUsage, args[0])
	}
}

func (a *App) executeAuth(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: auth requires add, login, list, use, or status", ErrUsage)
	}
	switch args[0] {
	case "add":
		if len(args) != 3 || args[1] != profile.ProviderCodex {
			return fmt.Errorf("%w: usage: auth add codex <profile>", ErrUsage)
		}
		return a.authAdd(ctx, args[1], args[2])
	case "login":
		if len(args) != 3 || args[1] != profile.ProviderCodex {
			return fmt.Errorf("%w: usage: auth login codex <profile>", ErrUsage)
		}
		return a.authLoginCodex(ctx, args[2])
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("%w: usage: auth list", ErrUsage)
		}
		return a.authList()
	case "use":
		if len(args) != 3 || args[1] != profile.ProviderCodex {
			return fmt.Errorf("%w: usage: auth use codex <profile>", ErrUsage)
		}
		return a.authUse(args[1], args[2])
	case "status":
		if len(args) < 2 || len(args) > 3 || args[1] != profile.ProviderCodex {
			return fmt.Errorf("%w: usage: auth status codex [profile]", ErrUsage)
		}
		requestedID := ""
		if len(args) == 3 {
			requestedID = args[2]
		}
		return a.authStatus(ctx, args[1], requestedID)
	default:
		return fmt.Errorf("%w: unknown auth command %q", ErrUsage, args[0])
	}
}

func (a *App) authAdd(ctx context.Context, provider, id string) error {
	registry, err := a.Store.Load()
	if err != nil {
		return err
	}
	p, err := newProfile(provider, id)
	if err != nil {
		return err
	}
	if _, exists := registry.Find(provider, id); exists {
		return fmt.Errorf("profile %s/%s already exists", provider, id)
	}

	switch provider {
	case profile.ProviderCodex:
		home := a.Store.CodexHome(id)
		if err := os.MkdirAll(home, 0o700); err != nil {
			return fmt.Errorf("create CODEX_HOME: %w", err)
		}
		fmt.Fprintf(a.Out, "Starting official Codex ChatGPT login for profile %q.\n", id)
		if err := a.Executor.Run(ctx, codexCommand(a.CodexBin, home, []string{"login"})); err != nil {
			return err
		}
	}

	if err := registry.Add(p); err != nil {
		return err
	}
	if err := a.Store.Save(registry); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Registered %s/%s. GPT Codex Router Auth Broker will use the official Codex OAuth state in this isolated profile.\n", provider, id)
	return nil
}

func (a *App) authLoginCodex(ctx context.Context, id string) error {
	registry, err := a.Store.Load()
	if err != nil {
		return err
	}
	if _, ok := registry.Find(profile.ProviderCodex, id); !ok {
		return fmt.Errorf("profile codex/%s not found", id)
	}
	home := a.Store.CodexHome(id)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create CODEX_HOME: %w", err)
	}
	fmt.Fprintf(a.Out, "Refreshing official Codex ChatGPT login for profile %q.\n", id)
	return a.Executor.Run(ctx, codexCommand(a.CodexBin, home, []string{"login"}))
}

func (a *App) authList() error {
	registry, err := a.Store.Load()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tPROFILE\tACTIVE\tISOLATION\tAUTH OWNER")
	for _, p := range registry.SortedProfiles() {
		active := ""
		if registry.Active[p.Provider] == p.ID {
			active = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Provider, p.ID, active, p.Isolation, "GPT Codex Router Auth Broker")
	}
	return tw.Flush()
}

func (a *App) authUse(provider, id string) error {
	registry, err := a.Store.Load()
	if err != nil {
		return err
	}
	if err := registry.Use(provider, id); err != nil {
		return err
	}
	if err := a.Store.Save(registry); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Active %s profile: %s\n", provider, id)
	if provider == profile.ProviderCodex {
		fmt.Fprintln(a.Out, "The gateway will use this profile for the next new backend request. A confirmed Codex usage-limit response may fail over automatically to another registered profile.")
	}
	return nil
}

func (a *App) authStatus(ctx context.Context, provider, requestedID string) error {
	registry, err := a.Store.Load()
	if err != nil {
		return err
	}
	p, err := selectProfile(registry, provider, requestedID)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Codex ChatGPT profile %s (CODEX_HOME=%s)\n", p.ID, a.Store.CodexHome(p.ID))
	return a.Executor.Run(ctx, codexCommand(a.CodexBin, a.Store.CodexHome(p.ID), []string{"login", "status"}))
}

type runOptions struct {
	profileID  string
	clientArgs []string
}

func parseRunOptions(args []string) (runOptions, error) {
	var opts runOptions
	for i := 0; i < len(args); {
		switch args[i] {
		case "--profile":
			if i+1 >= len(args) {
				return runOptions{}, fmt.Errorf("%w: --profile requires a value", ErrUsage)
			}
			opts.profileID = args[i+1]
			i += 2
		case "--":
			opts.clientArgs = append([]string(nil), args[i+1:]...)
			return opts, nil
		case "--surface":
			return runOptions{}, fmt.Errorf("%w: --surface was removed; Codex Desktop is no longer launched by GPT Codex Router", ErrUsage)
		default:
			opts.clientArgs = append([]string(nil), args[i:]...)
			return opts, nil
		}
	}
	return opts, nil
}

func (a *App) executeRun(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: usage: run codex [--profile <id>] [-- <client args...>]", ErrUsage)
	}
	provider := args[0]
	opts, err := parseRunOptions(args[1:])
	if err != nil {
		return err
	}
	registry, err := a.Store.Load()
	if err != nil {
		return err
	}
	p, err := selectProfile(registry, provider, opts.profileID)
	if err != nil {
		return err
	}
	if provider != profile.ProviderCodex {
		return fmt.Errorf("unsupported provider %q", provider)
	}
	return a.Executor.Run(ctx, codexCommand(a.CodexBin, a.Store.CodexHome(p.ID), opts.clientArgs))
}

func (a *App) executeServe(ctx context.Context, args []string) error {
	listen, err := parseServeArgs(args)
	if err != nil {
		return err
	}
	if err := requireSafeListenAddress(listen); err != nil {
		return err
	}
	broker := authbroker.New(a.Store)
	if _, err := broker.ValidateActive(); err != nil {
		return err
	}
	upstream, _ := url.Parse("https://chatgpt.com")
	handler, err := gateway.New(broker, upstream)
	if err != nil {
		return err
	}
	logPath := filepath.Join(a.Store.Root(), "observability", "events.jsonl")
	logger, err := observability.NewLogger(observability.LoggerConfig{
		Out: a.Out, FilePath: logPath, DailyDir: filepath.Join(a.Store.Root(), "observability", "daily"),
		ServiceVersion: a.Version, ServiceCommit: a.Commit,
	})
	if err != nil {
		return fmt.Errorf("initialize observability: %w", err)
	}
	defer logger.Close()
	handler.SetRequestLogger(logger.Emit)
	logger.Emit(observability.Event{EventType: observability.EventStartup})
	server := &http.Server{
		Addr:              listen,
		Handler:           serverHandler(handler),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func parseServeArgs(args []string) (string, error) {
	listen := defaultListenAddress
	for i := 0; i < len(args); i++ {
		if args[i] != "--listen" || i+1 >= len(args) || i+2 != len(args) {
			return "", fmt.Errorf("%w: usage: serve [--listen 127.0.0.1:8317]", ErrUsage)
		}
		listen = args[i+1]
		i++
	}
	return listen, nil
}

func requireSafeListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.TrimSpace(port) == "" {
		return fmt.Errorf("%w: invalid listen address %q", ErrUsage, address)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if os.Getenv("GPT_CODEX_ROUTER_CONTAINER") == "1" && ip != nil && ip.IsUnspecified() {
		return nil
	}
	return fmt.Errorf("refusing non-loopback listen address %q; use loopback, or container mode with an unspecified container address behind a host loopback port mapping", address)
}

func serverHandler(proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func newProfile(provider, id string) (profile.Profile, error) {
	if provider != profile.ProviderCodex {
		return profile.Profile{}, fmt.Errorf("unsupported provider %q", provider)
	}
	p := profile.Profile{ID: id, Provider: provider, Isolation: "codex-home"}
	if err := profile.ValidateProfile(p); err != nil {
		return profile.Profile{}, err
	}
	return p, nil
}

func selectProfile(registry *profile.Registry, provider, requestedID string) (profile.Profile, error) {
	if requestedID != "" {
		if p, ok := registry.Find(provider, requestedID); ok {
			return p, nil
		}
		return profile.Profile{}, fmt.Errorf("profile %s/%s not found", provider, requestedID)
	}
	if p, ok := registry.ActiveProfile(provider); ok {
		return p, nil
	}
	return profile.Profile{}, fmt.Errorf("no active %s profile; add one with `auth add %s <profile>`", provider, provider)
}

func codexCommand(executable, home string, args []string) process.Command {
	clientArgs := []string{"-c", `cli_auth_credentials_store="file"`}
	clientArgs = append(clientArgs, args...)
	return process.Command{
		Executable: executable,
		Args:       clientArgs,
		SetEnv:     map[string]string{"CODEX_HOME": filepath.Clean(home)},
		UnsetEnv:   codexCredentialOverrideEnv(),
	}
}

func codexCredentialOverrideEnv() []string {
	return []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"}
}

func (a *App) printUsage() {
	fmt.Fprintln(a.Out, strings.TrimSpace(`GPT Codex Router - local ChatGPT auth gateway

Usage:
  gpt-codex-router auth add codex <profile>
  gpt-codex-router auth login codex <profile>
  gpt-codex-router auth list
  gpt-codex-router auth use codex <profile>
  gpt-codex-router auth status codex [profile]
  gpt-codex-router serve [--listen 127.0.0.1:8317]
  gpt-codex-router report [--since 3h] [--timezone Asia/Seoul] [--file <jsonl>|--stdin]
  gpt-codex-router run codex [--profile <id>] [-- <client args...>]
  gpt-codex-router version

Codex Desktop setup:
  chatgpt_base_url = "http://127.0.0.1:8317/backend-api"
  openai_base_url = "http://127.0.0.1:8317/backend-api/codex"

The openai_base_url setting is required so both existing and new OpenAI-provider threads route Responses inference through GPT Codex Router. The gateway returns 426 only for the Responses WebSocket endpoint, causing Codex to fall back to HTTP while other backend WebSockets continue to proxy normally. The gateway accepts only /backend-api/* plus a local /healthz probe, ignores inbound auth, and injects auth from the selected Codex ChatGPT profile. Native mode is loopback-only. Container mode may listen on the container wildcard address only when GPT_CODEX_ROUTER_CONTAINER=1; publish that port to host loopback only. Confirmed subscription usage limits may fail over to another registered profile.`))
}
