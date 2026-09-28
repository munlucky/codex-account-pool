package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
)

type authRetryProvider struct {
	mu           sync.Mutex
	current      authbroker.Credentials
	refreshed    authbroker.Credentials
	refreshErr   error
	refreshCalls int
}

func (p *authRetryProvider) Credentials(context.Context) (authbroker.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current, nil
}

func (p *authRetryProvider) CredentialsForProfile(_ context.Context, profileID string) (authbroker.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current.ProfileID == profileID {
		return p.current, nil
	}
	return p.refreshed, nil
}

func (p *authRetryProvider) ForceRefresh(_ context.Context, profileID string) (authbroker.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshCalls++
	if p.refreshErr != nil {
		return authbroker.Credentials{}, p.refreshErr
	}
	if p.refreshed.ProfileID == profileID {
		p.current = p.refreshed
	}
	return p.refreshed, nil
}

func (p *authRetryProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshCalls
}

func TestUnauthorizedRefreshesSameProfileOnceAndRetries(t *testing.T) {
	provider := &authRetryProvider{
		current:   authbroker.Credentials{ProfileID: "one", AccessToken: "old-token", AccountID: "acct-one"},
		refreshed: authbroker.Credentials{ProfileID: "one", AccessToken: "new-token", AccountID: "acct-one"},
	}
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-one" {
			t.Fatalf("account id=%q", got)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer old-token":
			http.Error(w, "expired", http.StatusUnauthorized)
		case "Bearer new-token":
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected authorization header")
		}
	}))
	defer upstream.Close()

	h := mustHandler(t, provider, upstream.URL)
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/backend-api/codex/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if attempts != 2 || provider.calls() != 1 {
		t.Fatalf("attempts=%d refreshes=%d", attempts, provider.calls())
	}
}

func TestUnauthorizedDoesNotRefreshMoreThanOnce(t *testing.T) {
	provider := &authRetryProvider{
		current:   authbroker.Credentials{ProfileID: "one", AccessToken: "old-token", AccountID: "acct-one"},
		refreshed: authbroker.Credentials{ProfileID: "one", AccessToken: "new-token", AccountID: "acct-one"},
	}
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	h := mustHandler(t, provider, upstream.URL)
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/backend-api/codex/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if attempts != 2 || provider.calls() != 1 {
		t.Fatalf("attempts=%d refreshes=%d", attempts, provider.calls())
	}
}

func TestUnauthorizedDoesNotRetryUnknownMutation(t *testing.T) {
	provider := &authRetryProvider{
		current:   authbroker.Credentials{ProfileID: "one", AccessToken: "old-token", AccountID: "acct-one"},
		refreshed: authbroker.Credentials{ProfileID: "one", AccessToken: "new-token", AccountID: "acct-one"},
	}
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	h := mustHandler(t, provider, upstream.URL)
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/backend-api/unknown-mutation", strings.NewReader(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if attempts != 1 || provider.calls() != 0 {
		t.Fatalf("unsafe mutation retried: attempts=%d refreshes=%d", attempts, provider.calls())
	}
}

func TestSafeAuthRetryScope(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/backend-api/codex/models", true},
		{http.MethodHead, "/backend-api/codex/models", true},
		{http.MethodOptions, "/backend-api/codex/models", true},
		{http.MethodPost, "/backend-api/codex/responses", true},
		{http.MethodPost, "/backend-api/unknown-mutation", false},
		{http.MethodDelete, "/backend-api/codex/models", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "http://local"+tc.path, nil)
		if got := safeAuthRetry(req); got != tc.want {
			t.Fatalf("%s %s safe=%v want=%v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestUnauthorizedTemporaryRefreshFailureReturnsGatewayError(t *testing.T) {
	provider := &authRetryProvider{
		current:    authbroker.Credentials{ProfileID: "one", AccessToken: "old-token", AccountID: "acct-one"},
		refreshErr: &authbroker.AuthError{Code: authbroker.ErrorTemporarilyUnavailable, Op: "test refresh"},
	}
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	h := mustHandler(t, provider, upstream.URL)
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/backend-api/codex/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if attempts != 1 || provider.calls() != 1 {
		t.Fatalf("attempts=%d refreshes=%d", attempts, provider.calls())
	}
}

type pinnedFailoverProvider struct {
	mu            sync.Mutex
	active        authbroker.Credentials
	pinned        authbroker.Credentials
	fallback      authbroker.Credentials
	failoverCalls int
}

func (p *pinnedFailoverProvider) Credentials(context.Context) (authbroker.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active, nil
}

func (p *pinnedFailoverProvider) CredentialsForProfile(_ context.Context, profileID string) (authbroker.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pinned.ProfileID == profileID {
		return p.pinned, nil
	}
	return authbroker.Credentials{}, nil
}

func (p *pinnedFailoverProvider) ForceRefresh(_ context.Context, _ string) (authbroker.Credentials, error) {
	return authbroker.Credentials{}, nil
}

func (p *pinnedFailoverProvider) Failover(_ context.Context, _ string, _ time.Time) (authbroker.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failoverCalls++
	p.active = p.fallback
	return p.fallback, nil
}

func (p *pinnedFailoverProvider) failovers() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failoverCalls
}

func TestPinnedProfileUsageLimitDoesNotFailover(t *testing.T) {
	provider := &pinnedFailoverProvider{
		active:   authbroker.Credentials{ProfileID: "two", AccessToken: "token-two", AccountID: "acct-two"},
		pinned:   authbroker.Credentials{ProfileID: "one", AccessToken: "token-one", AccountID: "acct-one"},
		fallback: authbroker.Credentials{ProfileID: "two", AccessToken: "token-two", AccountID: "acct-two"},
	}
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-one" {
			t.Fatalf("pinned account id=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","resets_at":4102444800}}`))
	}))
	defer upstream.Close()

	h := mustHandler(t, provider, upstream.URL)
	req := httptest.NewRequest(http.MethodGet, "http://local/backend-api/codex/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTPForProfile(rr, req, "one")

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if attempts != 1 || provider.failovers() != 0 {
		t.Fatalf("pinned check switched profile: attempts=%d failovers=%d", attempts, provider.failovers())
	}
}
