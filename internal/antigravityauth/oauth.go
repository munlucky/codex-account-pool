package antigravityauth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	DefaultClientID       = ""
	DefaultClientSecret   = ""
	DefaultAuthURL        = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenURL       = "https://oauth2.googleapis.com/token"
	DefaultProjectURL     = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	DefaultOnboardURL     = "https://daily-cloudcode-pa.googleapis.com/v1internal:onboardUser"
	DefaultRedirectURI    = "http://127.0.0.1:51121/callback"
	AntigravityIDEVersion = "2.5.5"
)

var defaultScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

type Authenticator struct {
	Credentials             *Store
	Client                  *http.Client
	ClientID                string
	ClientSecret            string
	AuthURL                 string
	TokenURL                string
	ProjectURL              string
	OnboardURL              string
	RedirectURI             string
	UserAgent               string
	Now                     func() time.Time
	DisableLoopbackCallback bool
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func NewAuthenticatorForStore(store *Store) *Authenticator {
	return &Authenticator{
		Credentials:             store,
		Client:                  &http.Client{Timeout: 30 * time.Second},
		ClientID:                envOr("GOOGLE_ANTIGRAVITY_CLIENT_ID", DefaultClientID),
		ClientSecret:            envOr("GOOGLE_ANTIGRAVITY_CLIENT_SECRET", DefaultClientSecret),
		AuthURL:                 DefaultAuthURL,
		TokenURL:                DefaultTokenURL,
		ProjectURL:              DefaultProjectURL,
		OnboardURL:              DefaultOnboardURL,
		RedirectURI:             DefaultRedirectURI,
		UserAgent:               envOr("GOOGLE_ANTIGRAVITY_USER_AGENT", antigravityUserAgent()),
		Now:                     time.Now,
		DisableLoopbackCallback: os.Getenv("GPT_CODEX_ROUTER_CONTAINER") == "1",
	}
}

func (a *Authenticator) Login(ctx context.Context, profileID string, in io.Reader, out io.Writer, forceAccountSelect bool) error {
	if a == nil || a.Credentials == nil {
		return errors.New("google-antigravity authenticator is not configured")
	}
	if strings.TrimSpace(a.clientID()) == "" {
		return errors.New("GOOGLE_ANTIGRAVITY_CLIENT_ID is required for Google Antigravity login")
	}
	state, err := randomURLToken(32)
	if err != nil {
		return fmt.Errorf("generate OAuth state: %w", err)
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		return fmt.Errorf("generate PKCE verifier: %w", err)
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])

	redirectURI := a.redirectURI()
	var callback *callbackListener
	if !a.DisableLoopbackCallback {
		callback, _ = startCallbackListener(redirectURI, state)
	}
	if callback != nil {
		defer callback.Close()
	}

	values := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.clientID()},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(defaultScopes, " ")},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
	}
	if forceAccountSelect {
		values.Set("prompt", "consent select_account")
	}
	fmt.Fprintln(out, "Open this URL in your browser to sign in with Google Antigravity:")
	fmt.Fprintf(out, "%s?%s\n", a.authURL(), values.Encode())
	if callback != nil {
		fmt.Fprintln(out, "After authorization, the loopback callback should complete automatically. If it does not, paste the complete redirected URL here and press Enter.")
	} else {
		fmt.Fprintln(out, "Docker/headless login: after Google redirects to 127.0.0.1:51121, the browser may show a connection error. Copy the COMPLETE URL from the browser address bar, paste it here, and press Enter.")
	}

	code, err := waitForCode(ctx, callback, in, state)
	if err != nil {
		return fmt.Errorf("read Google OAuth redirect: %w", err)
	}
	fmt.Fprintln(out, "Authorization response accepted. Exchanging OAuth code...")
	payload, err := a.exchange(ctx, code, verifier, redirectURI)
	if err != nil {
		return fmt.Errorf("exchange Google OAuth code: %w", err)
	}
	fmt.Fprintln(out, "OAuth token exchange completed. Discovering Cloud Code Assist project...")
	projectID, err := a.discoverProject(ctx, payload.AccessToken)
	if err != nil {
		return fmt.Errorf("discover Cloud Code Assist project: %w", err)
	}
	fmt.Fprintln(out, "Cloud Code Assist project binding resolved. Saving profile credential...")
	refreshToken := payload.RefreshToken
	if refreshToken == "" {
		return errors.New("Antigravity token response did not include a refresh token")
	}
	expiresIn := payload.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	credential := Credential{
		AccessToken:  payload.AccessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    a.now().Add(time.Duration(expiresIn)*time.Second - 5*time.Minute),
		ProjectID:    projectID,
	}
	if err := a.Credentials.Save(profileID, credential); err != nil {
		return err
	}
	fmt.Fprintf(out, "Google Antigravity login completed for profile %q.\n", profileID)
	return nil
}

func (a *Authenticator) Refresh(ctx context.Context, profileID string, current Credential) (Credential, error) {
	if current.RefreshToken == "" {
		return Credential{}, errors.New("google-antigravity refresh token is missing")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {a.clientID()},
		"refresh_token": {current.RefreshToken},
	}
	if secret := a.clientSecret(); secret != "" {
		form.Set("client_secret", secret)
	}
	payload, err := a.postToken(ctx, form)
	if err != nil {
		return Credential{}, err
	}
	if payload.RefreshToken == "" {
		payload.RefreshToken = current.RefreshToken
	}
	projectID, discoverErr := a.discoverProject(ctx, payload.AccessToken)
	if discoverErr != nil {
		projectID = current.ProjectID
	}
	if projectID == "" {
		return Credential{}, errors.New("Antigravity refresh could not resolve a Cloud Code Assist project")
	}
	expiresIn := payload.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	updated := Credential{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, ExpiresAt: a.now().Add(time.Duration(expiresIn)*time.Second - 5*time.Minute), ProjectID: projectID}
	if err := a.Credentials.Save(profileID, updated); err != nil {
		return Credential{}, err
	}
	return updated, nil
}

func (a *Authenticator) exchange(ctx context.Context, code, verifier, redirectURI string) (tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {a.clientID()},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	if secret := a.clientSecret(); secret != "" {
		form.Set("client_secret", secret)
	}
	return a.postToken(ctx, form)
}

func (a *Authenticator) postToken(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return tokenResponse{}, errors.New("Antigravity token request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return tokenResponse{}, fmt.Errorf("Antigravity token request failed: HTTP %d", resp.StatusCode)
	}
	var payload tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return tokenResponse{}, errors.New("Antigravity token response was invalid")
	}
	if payload.AccessToken == "" {
		return tokenResponse{}, errors.New("Antigravity token response did not include an access token")
	}
	return payload, nil
}

func (a *Authenticator) discoverProject(ctx context.Context, accessToken string) (string, error) {
	if project, _ := a.loadProject(ctx, accessToken); project != "" {
		return project, nil
	}
	for attempt := 0; attempt < 5; attempt++ {
		project, retry, err := a.onboard(ctx, accessToken)
		if err != nil {
			return "", err
		}
		if project != "" {
			return project, nil
		}
		if !retry {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", errors.New("Antigravity login could not discover a Cloud Code Assist project for this account")
}

func (a *Authenticator) loadProject(ctx context.Context, accessToken string) (string, error) {
	body := strings.NewReader(`{"metadata":{"ideType":"ANTIGRAVITY"}}`)
	resp, err := a.doProjectRequest(ctx, a.projectURL(), accessToken, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil
	}
	var root map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&root) != nil {
		return "", nil
	}
	return extractProjectID(root), nil
}

func (a *Authenticator) onboard(ctx context.Context, accessToken string) (string, bool, error) {
	body := strings.NewReader(fmt.Sprintf(`{"tier_id":"free-tier","metadata":{"ide_type":"ANTIGRAVITY","ide_name":"antigravity","ide_version":%q}}`, AntigravityIDEVersion))
	resp, err := a.doProjectRequest(ctx, a.onboardURL(), accessToken, body)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return "", true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, nil
	}
	var root map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&root) != nil {
		return "", true, nil
	}
	if done, _ := root["done"].(bool); !done {
		return "", true, nil
	}
	if response, _ := root["response"].(map[string]any); response != nil {
		return extractProjectID(response), false, nil
	}
	return extractProjectID(root), false, nil
}

func (a *Authenticator) doProjectRequest(ctx context.Context, endpoint, accessToken string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", a.userAgent())
	return a.httpClient().Do(req)
}

func extractProjectID(root map[string]any) string {
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		switch value := root[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case map[string]any:
			if id, _ := value["id"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

type callbackListener struct {
	server   *http.Server
	listener net.Listener
	result   chan callbackResult
}
type callbackResult struct {
	code string
	err  error
}

func startCallbackListener(redirectURI, state string) (*callbackListener, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	c := &callbackListener{listener: listener, result: make(chan callbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc(u.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			c.deliver(callbackResult{err: errors.New("OAuth state mismatch")})
			http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
			return
		}
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		if code == "" {
			c.deliver(callbackResult{err: errors.New("authorization code missing")})
			http.Error(w, "Authorization code missing", http.StatusBadRequest)
			return
		}
		c.deliver(callbackResult{code: code})
		_, _ = io.WriteString(w, "Google Antigravity login completed. You can close this tab.\n")
	})
	c.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = c.server.Serve(listener) }()
	return c, nil
}
func (c *callbackListener) deliver(r callbackResult) {
	select {
	case c.result <- r:
	default:
	}
}
func (c *callbackListener) Close() {
	if c != nil && c.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.server.Shutdown(ctx)
	}
}

func waitForCode(ctx context.Context, callback *callbackListener, in io.Reader, state string) (string, error) {
	manual := make(chan callbackResult, 1)
	if in != nil {
		go func() {
			line, err := bufio.NewReader(in).ReadString('\n')
			if err != nil && len(line) == 0 {
				manual <- callbackResult{err: err}
				return
			}
			code, err := parseCallbackInput(strings.TrimSpace(line), state)
			manual <- callbackResult{code: code, err: err}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case result := <-manual:
			if result.err != nil {
				if result.err == io.EOF && callback == nil {
					return "", errors.New("OAuth callback was unavailable and no redirect URL was provided")
				}
				return "", result.err
			}
			if result.code == "" {
				return "", errors.New("authorization code is empty")
			}
			return result.code, nil
		case result := <-func() <-chan callbackResult {
			if callback != nil {
				return callback.result
			}
			return nil
		}():
			return result.code, result.err
		}
	}
}

func parseCallbackInput(input, expectedState string) (string, error) {
	if input == "" {
		return "", errors.New("authorization code is empty")
	}
	if u, err := url.Parse(input); err == nil && u.Scheme != "" && u.Host != "" {
		state := u.Query().Get("state")
		if state != expectedState {
			return "", errors.New("OAuth state mismatch")
		}
		code := strings.TrimSpace(u.Query().Get("code"))
		if code == "" {
			return "", errors.New("authorization code missing")
		}
		return code, nil
	}
	return input, nil
}

func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func UserAgent() string { return envOr("GOOGLE_ANTIGRAVITY_USER_AGENT", antigravityUserAgent()) }
func antigravityUserAgent() string {
	return fmt.Sprintf("antigravity/ide/%s (os_type=windows; arch=amd64; aidev_client; auth_method=oauth)", AntigravityIDEVersion)
}
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
func (a *Authenticator) httpClient() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}
func (a *Authenticator) clientID() string {
	if strings.TrimSpace(a.ClientID) != "" {
		return a.ClientID
	}
	return envOr("GOOGLE_ANTIGRAVITY_CLIENT_ID", DefaultClientID)
}
func (a *Authenticator) clientSecret() string {
	if strings.TrimSpace(a.ClientSecret) != "" {
		return a.ClientSecret
	}
	return envOr("GOOGLE_ANTIGRAVITY_CLIENT_SECRET", DefaultClientSecret)
}
func (a *Authenticator) authURL() string {
	if a.AuthURL != "" {
		return a.AuthURL
	}
	return DefaultAuthURL
}
func (a *Authenticator) tokenURL() string {
	if a.TokenURL != "" {
		return a.TokenURL
	}
	return DefaultTokenURL
}
func (a *Authenticator) projectURL() string {
	if a.ProjectURL != "" {
		return a.ProjectURL
	}
	return DefaultProjectURL
}
func (a *Authenticator) onboardURL() string {
	if a.OnboardURL != "" {
		return a.OnboardURL
	}
	return DefaultOnboardURL
}
func (a *Authenticator) redirectURI() string {
	if a.RedirectURI != "" {
		return a.RedirectURI
	}
	return DefaultRedirectURI
}
func (a *Authenticator) userAgent() string {
	if a.UserAgent != "" {
		return a.UserAgent
	}
	return antigravityUserAgent()
}
func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
