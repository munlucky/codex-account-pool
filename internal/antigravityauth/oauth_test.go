package antigravityauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

func TestParseCallbackInputValidatesState(t *testing.T) {
	code, err := parseCallbackInput("http://127.0.0.1:51121/callback?code=abc&state=expected", "expected")
	if err != nil || code != "abc" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if _, err := parseCallbackInput("http://127.0.0.1:51121/callback?code=abc&state=wrong", "expected"); err == nil {
		t.Fatal("expected OAuth state mismatch")
	}
}

func TestRefreshPreservesRefreshTokenAndRebindsProject(t *testing.T) {
	root := t.TempDir()
	credentialStore := NewStore(profile.NewStore(root))
	old := Credential{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute), ProjectID: "old-project"}
	if err := credentialStore.Save("google-1", old); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := r.Form.Get("refresh_token"); got != "old-refresh" {
				t.Fatalf("refresh_token=%q", got)
			}
			if got := r.Form.Get("client_secret"); got != "test-secret" {
				t.Fatalf("client_secret=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "expires_in": 3600})
		case "/load":
			if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("authorization=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": map[string]any{"id": "new-project"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	auth := &Authenticator{
		Credentials: credentialStore,
		Client:      server.Client(), ClientID: "client-id", ClientSecret: "test-secret", TokenURL: server.URL + "/token", ProjectURL: server.URL + "/load",
		OnboardURL: server.URL + "/onboard", UserAgent: "test-agent", Now: func() time.Time { return now },
	}
	updated, err := auth.Refresh(context.Background(), "google-1", old)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessToken != "new-access" || updated.RefreshToken != "old-refresh" || updated.ProjectID != "new-project" {
		t.Fatalf("updated=%#v", updated)
	}
	if !updated.ExpiresAt.After(now) {
		t.Fatalf("expires_at=%s", updated.ExpiresAt)
	}
}

func TestDiscoverProjectFallsBackToOnboarding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/load":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case "/onboard":
			_ = json.NewEncoder(w).Encode(map[string]any{"done": true, "response": map[string]any{"projectId": "onboarded-project"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	auth := &Authenticator{Client: server.Client(), ProjectURL: server.URL + "/load", OnboardURL: server.URL + "/onboard", UserAgent: "test-agent"}
	project, err := auth.discoverProject(context.Background(), "access")
	if err != nil {
		t.Fatal(err)
	}
	if project != "onboarded-project" {
		t.Fatalf("project=%q", project)
	}
}

func TestExchangeUsesPKCEAndClientSecret(t *testing.T) {
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		form = make(url.Values, len(r.Form))
		for key, values := range r.Form {
			form[key] = append([]string(nil), values...)
		}
		_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":3600}`))
	}))
	defer server.Close()
	auth := &Authenticator{Client: server.Client(), ClientID: "client", ClientSecret: "test-secret", TokenURL: server.URL}
	if _, err := auth.exchange(context.Background(), "code", "verifier", "http://127.0.0.1/callback"); err != nil {
		t.Fatal(err)
	}
	if form.Get("code_verifier") != "verifier" || form.Get("grant_type") != "authorization_code" {
		t.Fatalf("form=%v", form)
	}
	if strings.TrimSpace(form.Get("client_secret")) != "test-secret" {
		t.Fatalf("client secret was not forwarded")
	}
}

func TestWaitForCodeAcceptsCompleteRedirectURL(t *testing.T) {
	input := strings.NewReader("http://127.0.0.1:51121/callback?state=expected&code=test-code&scope=email\n")
	code, err := waitForCode(context.Background(), nil, input, "expected")
	if err != nil {
		t.Fatal(err)
	}
	if code != "test-code" {
		t.Fatalf("code=%q", code)
	}
}

func TestWaitForCodeRejectsManualStateMismatchImmediately(t *testing.T) {
	input := strings.NewReader("http://127.0.0.1:51121/callback?state=wrong&code=test-code\n")
	if _, err := waitForCode(context.Background(), nil, input, "expected"); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("err=%v", err)
	}
}

func TestContainerAuthenticatorDisablesLoopbackCallback(t *testing.T) {
	t.Setenv("GPT_CODEX_ROUTER_CONTAINER", "1")
	store := NewStore(profile.NewStore(t.TempDir()))
	auth := NewAuthenticatorForStore(store)
	if !auth.DisableLoopbackCallback {
		t.Fatal("container login must use manual redirect input instead of an unreachable container-loopback callback")
	}
}
