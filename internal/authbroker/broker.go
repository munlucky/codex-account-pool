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
	return b.CredentialsForProfile(ctx, p.ID)
}

// CredentialsForProfile acquires credentials for one registered Codex profile
// without changing the provider's active profile.
func (b *Broker) CredentialsForProfile(ctx context.Context, profileID string) (Credentials, error) {
	if b == nil || b.Store == nil {
		return Credentials{}, errors.New("auth broker is not configured")
	}
	if err := b.requireRegistered(profileID); err != nil {
		return Credentials{}, err
	}
	_, doc, err := b.loadProfile(profileID)
	if err != nil {
		return Credentials{}, err
	}
	if !tokenNeedsRefresh(doc.Tokens.AccessToken, b.now()) {
		return credentialsFrom(profileID, doc)
	}

	release, err := AcquireProfileWriteLock(ctx, b.Store, profileID)
	if err != nil {
		return Credentials{}, errorWithCode(ErrorTemporarilyUnavailable, "wait for profile auth writer", err)
	}
	defer release()

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

// ForceRefresh refreshes the same profile once regardless of access-token
// expiry. It is used only after an upstream 401 and never changes the active
// profile.
func (b *Broker) ForceRefresh(ctx context.Context, profileID string) (Credentials, error) {
	if b == nil || b.Store == nil {
		return Credentials{}, errors.New("auth broker is not configured")
	}
	if err := b.requireRegistered(profileID); err != nil {
		return Credentials{}, err
	}
	release, err := AcquireProfileWriteLock(ctx, b.Store, profileID)
	if err != nil {
		return Credentials{}, errorWithCode(ErrorTemporarilyUnavailable, "wait for profile auth writer", err)
	}
	defer release()
	_, doc, err := b.loadProfile(profileID)
	if err != nil {
		return Credentials{}, err
	}
	if err := b.refresh(ctx, profileID, doc); err != nil {
		return Credentials{}, err
	}
	_, doc, err = b.loadProfile(profileID)
	if err != nil {
		return Credentials{}, err
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
			if creds, credErr := b.CredentialsForProfile(ctx, active.ID); credErr == nil {
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
		creds, credErr := b.CredentialsForProfile(ctx, candidate)
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

type ProfileAuthInfo struct {
	ProfileID       string
	AccountID       string
	HasAuth         bool
	HasAccessToken  bool
	HasRefreshToken bool
	HasAccountID    bool
	AccessExpiresAt time.Time
	AccessExpired   bool
}

func (b *Broker) InspectProfile(profileID string) (ProfileAuthInfo, error) {
	if b == nil || b.Store == nil {
		return ProfileAuthInfo{ProfileID: profileID}, errors.New("auth broker is not configured")
	}
	if err := b.requireRegistered(profileID); err != nil {
		return ProfileAuthInfo{ProfileID: profileID}, err
	}
	info, err := b.inspectAuthFile(profileID)
	if err != nil && Code(err) == ErrorNotLoggedIn {
		return ProfileAuthInfo{ProfileID: profileID}, nil
	}
	return info, err
}

// ValidateProfileAuth validates a Codex auth file without requiring that the
// profile is already registered. The administrator login flow uses it after a
// new device-code login and before registry mutation.
func (b *Broker) ValidateProfileAuth(profileID string) (ProfileAuthInfo, error) {
	if b == nil || b.Store == nil {
		return ProfileAuthInfo{ProfileID: profileID}, errors.New("auth broker is not configured")
	}
	if err := profile.ValidateProfile(profile.Profile{ID: profileID, Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		return ProfileAuthInfo{ProfileID: profileID}, err
	}
	info, err := b.inspectAuthFile(profileID)
	if err != nil {
		return info, err
	}
	if !info.HasAccessToken || !info.HasAccountID {
		return info, errorWithCode(ErrorNotLoggedIn, "validate codex auth", errors.New("required credentials are missing"))
	}
	if !info.HasRefreshToken {
		return info, errorWithCode(ErrorReauthRequired, "validate codex auth", errors.New("refresh token is missing"))
	}
	return info, nil
}

func (b *Broker) inspectAuthFile(profileID string) (ProfileAuthInfo, error) {
	info := ProfileAuthInfo{ProfileID: profileID}
	_, doc, err := b.loadProfile(profileID)
	if err != nil {
		return info, err
	}
	info.HasAuth = true
	info.HasAccessToken = strings.TrimSpace(doc.Tokens.AccessToken) != ""
	info.HasRefreshToken = strings.TrimSpace(doc.Tokens.RefreshToken) != ""
	info.AccountID = strings.TrimSpace(doc.Tokens.AccountID)
	info.HasAccountID = info.AccountID != ""
	if exp, ok := jwtUnixClaim(doc.Tokens.AccessToken, "exp"); ok {
		info.AccessExpiresAt = time.Unix(exp, 0).UTC()
		info.AccessExpired = !info.AccessExpiresAt.After(b.now())
	}
	if info.HasAccountID {
		for _, token := range []string{doc.Tokens.IDToken, doc.Tokens.AccessToken} {
			if tokenAccountID := jwtChatGPTAccountID(token); tokenAccountID != "" && tokenAccountID != doc.Tokens.AccountID {
				return info, errorWithCode(ErrorReauthRequired, "validate codex auth", fmt.Errorf("profile %s account identity mismatch", profileID))
			}
		}
	}
	return info, nil
}

func (b *Broker) requireRegistered(profileID string) error {
	registry, err := b.Store.Load()
	if err != nil {
		return err
	}
	if _, ok := registry.Find(profile.ProviderCodex, profileID); !ok {
		return fmt.Errorf("profile codex/%s not found", profileID)
	}
	return nil
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
			return nil, authFile{}, errorWithCode(ErrorNotLoggedIn, "load codex auth", fmt.Errorf("profile %s has no auth state", profileID))
		}
		return nil, authFile{}, fmt.Errorf("read codex auth profile %s: %w", profileID, err)
	}
	var doc authFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, authFile{}, errorWithCode(ErrorReauthRequired, "decode codex auth", err)
	}
	if !strings.EqualFold(strings.TrimSpace(doc.AuthMode), "chatgpt") {
		return nil, authFile{}, errorWithCode(ErrorNotLoggedIn, "load codex auth", fmt.Errorf("profile %s is not ChatGPT OAuth", profileID))
	}
	return data, doc, nil
}

func credentialsFrom(profileID string, doc authFile) (Credentials, error) {
	if strings.TrimSpace(doc.Tokens.AccessToken) == "" {
		return Credentials{}, errorWithCode(ErrorNotLoggedIn, "load codex credentials", fmt.Errorf("profile %s has no access token", profileID))
	}
	if strings.TrimSpace(doc.Tokens.AccountID) == "" {
		return Credentials{}, errorWithCode(ErrorNotLoggedIn, "load codex credentials", fmt.Errorf("profile %s has no account id", profileID))
	}
	return Credentials{ProfileID: profileID, AccessToken: doc.Tokens.AccessToken, AccountID: doc.Tokens.AccountID}, nil
}

func (b *Broker) refresh(ctx context.Context, profileID string, doc authFile) error {
	if strings.TrimSpace(doc.Tokens.RefreshToken) == "" {
		return errorWithCode(ErrorReauthRequired, "refresh codex credentials", fmt.Errorf("profile %s has no refresh token", profileID))
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
		return errorWithCode(ErrorTemporarilyUnavailable, "refresh codex credentials", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if definitiveRefreshRejection(resp.StatusCode, responseBody) {
			return errorWithCode(ErrorReauthRequired, "refresh codex credentials", fmt.Errorf("authority rejected refresh with HTTP %d", resp.StatusCode))
		}
		return errorWithCode(ErrorTemporarilyUnavailable, "refresh codex credentials", fmt.Errorf("authority returned HTTP %d", resp.StatusCode))
	}
	var refreshed refreshResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&refreshed); err != nil {
		return errorWithCode(ErrorTemporarilyUnavailable, "refresh codex credentials", err)
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" {
		return errorWithCode(ErrorTemporarilyUnavailable, "refresh codex credentials", errors.New("authority response contained no access token"))
	}
	for _, token := range []string{refreshed.IDToken, refreshed.AccessToken} {
		if refreshedAccountID := jwtChatGPTAccountID(token); refreshedAccountID != "" && refreshedAccountID != doc.Tokens.AccountID {
			return errorWithCode(ErrorReauthRequired, "refresh codex credentials", fmt.Errorf("profile %s account identity changed", profileID))
		}
	}
	return b.persistRefresh(profileID, refreshed)
}

func definitiveRefreshRejection(status int, body []byte) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
	default:
		return false
	}

	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Error) == 0 {
		return false
	}
	var code string
	if json.Unmarshal(payload.Error, &code) != nil {
		var nested struct {
			Code string `json:"code"`
			Type string `json:"type"`
		}
		if json.Unmarshal(payload.Error, &nested) != nil {
			return false
		}
		code = nested.Code
		if code == "" {
			code = nested.Type
		}
	}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_grant", "invalid_refresh_token", "refresh_token_expired", "refresh_token_reused":
		return true
	default:
		return false
	}
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
