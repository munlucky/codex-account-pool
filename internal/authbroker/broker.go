package authbroker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

const (
	DefaultRefreshURL = "https://auth.openai.com/oauth/token"
	DefaultClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
)

const refreshWindow = 5 * time.Minute

type Credentials struct {
	ProfileID   string
	AccessToken string
	AccountID   string
}

type Broker struct {
	Store      *profile.Store
	Client     *http.Client
	RefreshURL string
	ClientID   string
	Now        func() time.Time

	mu        sync.Mutex
	routeMu   sync.Mutex
	exhausted map[string]time.Time
}

type authFile struct {
	AuthMode string     `json:"auth_mode"`
	Tokens   authTokens `json:"tokens"`
}

type authTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type refreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

type refreshResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func New(store *profile.Store) *Broker {
	return &Broker{
		Store:      store,
		Client:     &http.Client{Timeout: 15 * time.Second},
		RefreshURL: DefaultRefreshURL,
		ClientID:   DefaultClientID,
		Now:        time.Now,
	}
}

func (b *Broker) ValidateActive() (string, error) {
	p, doc, err := b.loadActive()
	if err != nil {
		return "", err
	}
	if _, err := credentialsFrom(p.ID, doc); err != nil {
		return "", err
	}
	return p.ID, nil
}

func (b *Broker) Credentials(ctx context.Context) (Credentials, error) {
	if b == nil || b.Store == nil {
		return Credentials{}, errors.New("auth broker is not configured")
	}
	p, _, err := b.loadActive()
	if err != nil {
		return Credentials{}, err
	}
	return b.credentialsFor(ctx, p.ID)
}

func (b *Broker) credentialsFor(ctx context.Context, profileID string) (Credentials, error) {
	_, doc, err := b.loadProfile(profileID)
	if err != nil {
		return Credentials{}, err
	}
	if !tokenNeedsRefresh(doc.Tokens.AccessToken, b.now()) {
		return credentialsFrom(profileID, doc)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	_, doc, err = b.loadProfile(profileID)
	if err != nil {
		return Credentials{}, err
	}
	if tokenNeedsRefresh(doc.Tokens.AccessToken, b.now()) {
		if err := b.refresh(ctx, profileID, doc); err != nil {
			return Credentials{}, err
		}
		_, doc, err = b.loadProfile(profileID)
		if err != nil {
			return Credentials{}, err
		}
	}
	return credentialsFrom(profileID, doc)
}

func (b *Broker) Failover(ctx context.Context, exhaustedProfileID string, resetAt time.Time) (Credentials, error) {
	if b == nil || b.Store == nil {
		return Credentials{}, errors.New("auth broker is not configured")
	}
	b.routeMu.Lock()
	defer b.routeMu.Unlock()

	now := b.now()
	if b.exhausted == nil {
		b.exhausted = map[string]time.Time{}
	}
	for id, until := range b.exhausted {
		if !until.After(now) {
			delete(b.exhausted, id)
		}
	}
	if !resetAt.After(now) {
		resetAt = now.Add(time.Hour)
	}
	b.exhausted[exhaustedProfileID] = resetAt

	registry, err := b.Store.Load()
	if err != nil {
		return Credentials{}, err
	}
	if active, ok := registry.ActiveProfile(profile.ProviderCodex); ok && active.ID != exhaustedProfileID {
		if until, blocked := b.exhausted[active.ID]; !blocked || !until.After(now) {
			if creds, credErr := b.credentialsFor(ctx, active.ID); credErr == nil {
				return creds, nil
			}
		}
	}

	profiles := registry.SortedProfiles()
	ids := make([]string, 0, len(profiles))
	for _, p := range profiles {
		if p.Provider == profile.ProviderCodex {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) < 2 {
		return Credentials{}, fmt.Errorf("no alternate codex profile is available after %s reached its usage limit", exhaustedProfileID)
	}
	start := 0
	for i, id := range ids {
		if id == exhaustedProfileID {
			start = i
			break
		}
	}
	var lastErr error
	for offset := 1; offset < len(ids); offset++ {
		candidate := ids[(start+offset)%len(ids)]
		if until, blocked := b.exhausted[candidate]; blocked && until.After(now) {
			continue
		}
		creds, credErr := b.credentialsFor(ctx, candidate)
		if credErr != nil {
			lastErr = credErr
			continue
		}
		if err := registry.Use(profile.ProviderCodex, candidate); err != nil {
			return Credentials{}, err
		}
		if err := b.Store.Save(registry); err != nil {
			return Credentials{}, err
		}
		return creds, nil
	}
	if lastErr != nil {
		return Credentials{}, fmt.Errorf("no usable alternate codex profile after %s reached its usage limit: %w", exhaustedProfileID, lastErr)
	}
	return Credentials{}, fmt.Errorf("all alternate codex profiles are usage-limited")
}

func (b *Broker) loadActive() (profile.Profile, authFile, error) {
	registry, err := b.Store.Load()
	if err != nil {
		return profile.Profile{}, authFile{}, err
	}
	p, ok := registry.ActiveProfile(profile.ProviderCodex)
	if !ok {
		return profile.Profile{}, authFile{}, errors.New("no active codex profile; add one with `gpt-codex-router auth add codex <profile>`")
	}
	_, doc, err := b.loadProfile(p.ID)
	return p, doc, err
}

func (b *Broker) loadProfile(profileID string) ([]byte, authFile, error) {
	path := b.Store.CodexAuthPath(profileID)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, authFile{}, fmt.Errorf("codex profile %s has no auth state; run `gpt-codex-router auth login codex %s`", profileID, profileID)
		}
		return nil, authFile{}, fmt.Errorf("read codex auth profile %s: %w", profileID, err)
	}
	var doc authFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, authFile{}, fmt.Errorf("decode codex auth profile %s: %w", profileID, err)
	}
	if !strings.EqualFold(strings.TrimSpace(doc.AuthMode), "chatgpt") {
		return nil, authFile{}, fmt.Errorf("codex profile %s is not ChatGPT OAuth auth", profileID)
	}
	return data, doc, nil
}

func credentialsFrom(profileID string, doc authFile) (Credentials, error) {
	if strings.TrimSpace(doc.Tokens.AccessToken) == "" {
		return Credentials{}, fmt.Errorf("codex profile %s has no access token; run `gpt-codex-router auth login codex %s`", profileID, profileID)
	}
	if strings.TrimSpace(doc.Tokens.AccountID) == "" {
		return Credentials{}, fmt.Errorf("codex profile %s has no ChatGPT account id; run `gpt-codex-router auth login codex %s`", profileID, profileID)
	}
	return Credentials{ProfileID: profileID, AccessToken: doc.Tokens.AccessToken, AccountID: doc.Tokens.AccountID}, nil
}

func (b *Broker) refresh(ctx context.Context, profileID string, doc authFile) error {
	if strings.TrimSpace(doc.Tokens.RefreshToken) == "" {
		return fmt.Errorf("codex profile %s has no refresh token; run `gpt-codex-router auth login codex %s`", profileID, profileID)
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	refreshURL := strings.TrimSpace(b.RefreshURL)
	if refreshURL == "" {
		refreshURL = DefaultRefreshURL
	}
	clientID := strings.TrimSpace(b.ClientID)
	if clientID == "" {
		clientID = DefaultClientID
	}
	body, err := json.Marshal(refreshRequest{ClientID: clientID, GrantType: "refresh_token", RefreshToken: doc.Tokens.RefreshToken})
	if err != nil {
		return fmt.Errorf("encode codex token refresh: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create codex token refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("refresh codex profile %s: transport failure", profileID)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("refresh codex profile %s: authority rejected refresh (HTTP %d); run `gpt-codex-router auth login codex %s`", profileID, resp.StatusCode, profileID)
	}
	var refreshed refreshResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&refreshed); err != nil {
		return fmt.Errorf("refresh codex profile %s: invalid authority response", profileID)
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" {
		return fmt.Errorf("refresh codex profile %s: authority response contained no access token", profileID)
	}
	for _, token := range []string{refreshed.IDToken, refreshed.AccessToken} {
		if refreshedAccountID := jwtChatGPTAccountID(token); refreshedAccountID != "" && refreshedAccountID != doc.Tokens.AccountID {
			return fmt.Errorf("refresh codex profile %s: account identity changed; run `gpt-codex-router auth login codex %s`", profileID, profileID)
		}
	}
	return b.persistRefresh(profileID, refreshed)
}

func (b *Broker) persistRefresh(profileID string, refreshed refreshResponse) error {
	path := b.Store.CodexAuthPath(profileID)
	data, _, err := b.loadProfile(profileID)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("decode codex auth profile %s for refresh: %w", profileID, err)
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(root["tokens"], &tokens); err != nil {
		return fmt.Errorf("decode codex token set for profile %s: %w", profileID, err)
	}
	setString := func(key, value string) error {
		encoded, err := json.Marshal(value)
		if err == nil {
			tokens[key] = encoded
		}
		return err
	}
	if err := setString("access_token", refreshed.AccessToken); err != nil {
		return err
	}
	if refreshed.RefreshToken != "" {
		if err := setString("refresh_token", refreshed.RefreshToken); err != nil {
			return err
		}
	}
	if refreshed.IDToken != "" {
		if err := setString("id_token", refreshed.IDToken); err != nil {
			return err
		}
	}
	encodedTokens, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("encode codex token set for profile %s: %w", profileID, err)
	}
	root["tokens"] = encodedTokens
	lastRefresh, _ := json.Marshal(b.now().UTC().Format(time.RFC3339Nano))
	root["last_refresh"] = lastRefresh
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode codex auth profile %s: %w", profileID, err)
	}
	encoded = append(encoded, '\n')
	return atomicWrite(path, encoded)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create auth directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create auth temp file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure auth temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write auth temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync auth temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close auth temp file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace auth file: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

func tokenNeedsRefresh(token string, now time.Time) bool {
	exp, ok := jwtUnixClaim(token, "exp")
	if !ok {
		return false
	}
	return !time.Unix(exp, 0).After(now.Add(refreshWindow))
}

func jwtUnixClaim(token, claim string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var values map[string]any
	if err := json.Unmarshal(payload, &values); err != nil {
		return 0, false
	}
	value, ok := values[claim].(float64)
	if !ok {
		return 0, false
	}
	return int64(value), true
}

func jwtChatGPTAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var values map[string]any
	if err := json.Unmarshal(payload, &values); err != nil {
		return ""
	}
	if value, _ := values["chatgpt_account_id"].(string); value != "" {
		return value
	}
	auth, _ := values["https://api.openai.com/auth"].(map[string]any)
	value, _ := auth["chatgpt_account_id"].(string)
	return value
}

func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}
