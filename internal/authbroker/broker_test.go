package authbroker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

func TestCredentialsFollowActiveProfileOnEveryRequest(t *testing.T) {
	store := testStore(t, "one", "two")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(time.Hour), "acct-one"), "refresh-one", "acct-one")
	writeAuth(t, store, "two", jwt(t, now.Add(time.Hour), "acct-two"), "refresh-two", "acct-two")
	broker := New(store)
	broker.Now = func() time.Time { return now }

	first, err := broker.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.ProfileID != "one" || first.AccountID != "acct-one" {
		t.Fatalf("first=%+v", first)
	}
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Use(profile.ProviderCodex, "two"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}
	second, err := broker.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.ProfileID != "two" || second.AccountID != "acct-two" {
		t.Fatalf("second=%+v", second)
	}
}

func TestCredentialsRefreshNearExpiryAndPersistOfficialShape(t *testing.T) {
	store := testStore(t, "one")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(time.Minute), "acct-one"), "refresh-old", "acct-one")
	newAccess := jwt(t, now.Add(time.Hour), "acct-one")
	newID := idJWT(t, "acct-one")

	var refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected refresh request: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var body refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.ClientID != DefaultClientID || body.GrantType != "refresh_token" || body.RefreshToken != "refresh-old" {
			t.Fatalf("refresh body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(refreshResponse{IDToken: newID, AccessToken: newAccess, RefreshToken: "refresh-new"})
	}))
	defer server.Close()

	broker := New(store)
	broker.Now = func() time.Time { return now }
	broker.RefreshURL = server.URL
	creds, err := broker.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshCalls != 1 || creds.AccessToken != newAccess || creds.AccountID != "acct-one" {
		t.Fatalf("calls=%d creds=%+v", refreshCalls, creds)
	}

	data, err := os.ReadFile(store.CodexAuthPath("one"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		AuthMode    string     `json:"auth_mode"`
		Tokens      authTokens `json:"tokens"`
		LastRefresh string     `json:"last_refresh"`
		Extra       string     `json:"extra"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.AuthMode != "chatgpt" || persisted.Tokens.AccessToken != newAccess || persisted.Tokens.RefreshToken != "refresh-new" || persisted.Extra != "preserve-me" || persisted.LastRefresh == "" {
		t.Fatalf("persisted refresh is incomplete: %+v", persisted)
	}
}

func TestRefreshRejectsAccountIdentityChange(t *testing.T) {
	store := testStore(t, "one")
	now := time.Unix(2_000_000_000, 0)
	oldAccess := jwt(t, now.Add(time.Minute), "acct-one")
	writeAuth(t, store, "one", oldAccess, "refresh-old", "acct-one")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(refreshResponse{
			IDToken:     idJWT(t, "different-account"),
			AccessToken: jwt(t, now.Add(time.Hour), "different-account"),
		})
	}))
	defer server.Close()

	broker := New(store)
	broker.Now = func() time.Time { return now }
	broker.RefreshURL = server.URL
	_, err := broker.Credentials(context.Background())
	if err == nil || !strings.Contains(err.Error(), "account identity changed") {
		t.Fatalf("expected identity mismatch, got %v", err)
	}
	data, err := os.ReadFile(store.CodexAuthPath("one"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), oldAccess) {
		t.Fatal("failed refresh must not overwrite the previous credential set")
	}
}

func TestRefreshErrorDoesNotEchoAuthorityBody(t *testing.T) {
	store := testStore(t, "one")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(time.Minute), "acct-one"), "refresh-old", "acct-one")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sensitive-provider-detail", http.StatusUnauthorized)
	}))
	defer server.Close()

	broker := New(store)
	broker.Now = func() time.Time { return now }
	broker.RefreshURL = server.URL
	_, err := broker.Credentials(context.Background())
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if strings.Contains(err.Error(), "sensitive-provider-detail") || strings.Contains(err.Error(), "refresh-old") {
		t.Fatalf("provider or credential detail leaked: %v", err)
	}
}

func TestValidateActiveRequiresChatGPTOAuth(t *testing.T) {
	store := testStore(t, "one")
	if err := os.MkdirAll(store.CodexHome("one"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.CodexAuthPath("one"), []byte(`{"auth_mode":"apikey","tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := New(store)
	if _, err := broker.ValidateActive(); err == nil || !strings.Contains(err.Error(), "not ChatGPT OAuth") {
		t.Fatalf("unexpected validation result: %v", err)
	}
}

func testStore(t *testing.T, ids ...string) *profile.Store {
	t.Helper()
	store := profile.NewStore(t.TempDir())
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := registry.Add(profile.Profile{ID: id, Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}
	return store
}

func writeAuth(t *testing.T, store *profile.Store, id, access, refresh, account string) {
	t.Helper()
	if err := os.MkdirAll(store.CodexHome(id), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      idJWT(t, account),
			"access_token":  access,
			"refresh_token": refresh,
			"account_id":    account,
		},
		"last_refresh": "2026-01-01T00:00:00Z",
		"extra":        "preserve-me",
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.CodexAuthPath(id), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func jwt(t *testing.T, exp time.Time, account string) string {
	t.Helper()
	return makeJWT(t, map[string]any{
		"exp":                         exp.Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account},
	})
}

func idJWT(t *testing.T, account string) string {
	t.Helper()
	return makeJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account},
	})
}

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("e30.%s.sig", base64.RawURLEncoding.EncodeToString(payload))
}

func TestFailoverSwitchesToNextProfileAndSkipsExhaustedProfiles(t *testing.T) {
	store := testStore(t, "one", "two")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(time.Hour), "acct-one"), "refresh-one", "acct-one")
	writeAuth(t, store, "two", jwt(t, now.Add(time.Hour), "acct-two"), "refresh-two", "acct-two")
	broker := New(store)
	broker.Now = func() time.Time { return now }

	creds, err := broker.Failover(context.Background(), "one", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if creds.ProfileID != "two" || creds.AccountID != "acct-two" {
		t.Fatalf("fallback=%+v", creds)
	}
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	active, ok := registry.ActiveProfile(profile.ProviderCodex)
	if !ok || active.ID != "two" {
		t.Fatalf("active=%+v ok=%v", active, ok)
	}

	if _, err := broker.Failover(context.Background(), "two", now.Add(30*time.Minute)); err == nil || !strings.Contains(err.Error(), "usage-limited") {
		t.Fatalf("expected all profiles exhausted, got %v", err)
	}
}

func TestFailoverAllowsProfileAgainAfterReset(t *testing.T) {
	store := testStore(t, "one", "two")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(3*time.Hour), "acct-one"), "refresh-one", "acct-one")
	writeAuth(t, store, "two", jwt(t, now.Add(3*time.Hour), "acct-two"), "refresh-two", "acct-two")
	broker := New(store)
	broker.Now = func() time.Time { return now }

	if _, err := broker.Failover(context.Background(), "one", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	creds, err := broker.Failover(context.Background(), "two", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if creds.ProfileID != "one" {
		t.Fatalf("expected reset profile one, got %+v", creds)
	}
}
