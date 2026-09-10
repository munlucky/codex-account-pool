package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
)

type CredentialProvider interface {
	Credentials(context.Context) (authbroker.Credentials, error)
}

type failoverCredentialProvider interface {
	CredentialProvider
	Failover(context.Context, string, time.Time) (authbroker.Credentials, error)
}

type credentialContextKey struct{}

type RequestEvent struct {
	Method            string
	ProfileID         string
	StatusCode        int
	SwitchFrom        string
	SwitchTo          string
	TransportFallback bool
}

type RequestLogger func(RequestEvent)

type Handler struct {
	provider CredentialProvider
	proxy    *httputil.ReverseProxy
	logger   RequestLogger
}

type quotaFailoverTransport struct {
	base     http.RoundTripper
	provider CredentialProvider
	onSwitch func(string, string)
}

func New(provider CredentialProvider, upstream *url.URL) (*Handler, error) {
	if provider == nil {
		return nil, fmt.Errorf("credential provider is required")
	}
	if upstream == nil || (upstream.Scheme != "https" && upstream.Scheme != "http") || strings.TrimSpace(upstream.Host) == "" {
		return nil, fmt.Errorf("http(s) upstream is required")
	}
	h := &Handler{provider: provider}
	transport := &quotaFailoverTransport{
		base:     http.DefaultTransport,
		provider: provider,
		onSwitch: h.emitSwitch,
	}
	h.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			sanitizeAuthHeaders(pr.Out.Header)
			if creds, ok := pr.In.Context().Value(credentialContextKey{}).(authbroker.Credentials); ok {
				applyCredentials(pr.Out.Header, creds)
			}
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			h.emit(resp.Request, resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			h.emit(r, http.StatusBadGateway)
			http.Error(w, "ChatGPT upstream unavailable", http.StatusBadGateway)
		},
	}
	return h, nil
}

func (h *Handler) SetRequestLogger(logger RequestLogger) {
	h.logger = logger
}

func (h *Handler) emit(r *http.Request, statusCode int) {
	if h.logger == nil || r == nil {
		return
	}
	profileID := "-"
	if creds, ok := r.Context().Value(credentialContextKey{}).(authbroker.Credentials); ok && creds.ProfileID != "" {
		profileID = creds.ProfileID
	}
	h.logger(RequestEvent{Method: r.Method, ProfileID: profileID, StatusCode: statusCode})
}

func (h *Handler) emitSwitch(from, to string) {
	if h.logger == nil {
		return
	}
	h.logger(RequestEvent{SwitchFrom: from, SwitchTo: to})
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/backend-api" && !strings.HasPrefix(r.URL.Path, "/backend-api/") {
		http.NotFound(w, r)
		return
	}
	if isResponsesWebsocketUpgrade(r) {
		if h.logger != nil {
			h.logger(RequestEvent{Method: r.Method, StatusCode: http.StatusUpgradeRequired, TransportFallback: true})
		}
		w.Header().Set("Connection", "close")
		http.Error(w, "GPT Codex Router requires HTTP transport for quota-aware Responses routing", http.StatusUpgradeRequired)
		return
	}
	creds, err := h.provider.Credentials(r.Context())
	if err != nil {
		h.emit(r, http.StatusBadGateway)
		http.Error(w, "GPT Codex Router authentication unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	ctx := context.WithValue(r.Context(), credentialContextKey{}, creds)
	h.proxy.ServeHTTP(w, r.Clone(ctx))
}

func isResponsesWebsocketUpgrade(r *http.Request) bool {
	return r != nil &&
		r.URL.Path == "/backend-api/codex/responses" &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

func (t *quotaFailoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := makeReplayable(req)
	if err != nil {
		return nil, err
	}
	attempted := map[string]bool{}
	if creds, ok := requestCredentials(req); ok {
		attempted[creds.ProfileID] = true
	}

	current := req
	for {
		resp, err := t.base.RoundTrip(current)
		if err != nil {
			return nil, err
		}
		resp.Request = current
		limited, resetAt, err := usageLimitResponse(resp)
		if err != nil || !limited {
			return resp, err
		}
		provider, ok := t.provider.(failoverCredentialProvider)
		currentCreds, hasCreds := requestCredentials(current)
		if !ok || !hasCreds || currentCreds.ProfileID == "" {
			return resp, nil
		}
		fallback, failoverErr := provider.Failover(current.Context(), currentCreds.ProfileID, resetAt)
		if failoverErr != nil || fallback.ProfileID == "" || attempted[fallback.ProfileID] {
			return resp, nil
		}
		attempted[fallback.ProfileID] = true
		_ = resp.Body.Close()
		if t.onSwitch != nil {
			t.onSwitch(currentCreds.ProfileID, fallback.ProfileID)
		}
		current = cloneForRetry(current, body, fallback)
	}
}

func makeReplayable(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("buffer request for quota failover: %w", err)
	}
	_ = req.Body.Close()
	setRequestBody(req, body)
	return body, nil
}

func cloneForRetry(req *http.Request, body []byte, creds authbroker.Credentials) *http.Request {
	ctx := context.WithValue(req.Context(), credentialContextKey{}, creds)
	retry := req.Clone(ctx)
	retry.Header = req.Header.Clone()
	sanitizeAuthHeaders(retry.Header)
	applyCredentials(retry.Header, creds)
	if req.Body != nil || body != nil {
		setRequestBody(retry, body)
	}
	return retry
}

func setRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func sanitizeAuthHeaders(header http.Header) {
	header.Del("Authorization")
	header.Del("ChatGPT-Account-ID")
	header.Del("Proxy-Authorization")
	header.Del("Cookie")
}

func applyCredentials(header http.Header, creds authbroker.Credentials) {
	header.Set("Authorization", "Bearer "+creds.AccessToken)
	header.Set("ChatGPT-Account-ID", creds.AccountID)
}

func requestCredentials(req *http.Request) (authbroker.Credentials, bool) {
	creds, ok := req.Context().Value(credentialContextKey{}).(authbroker.Credentials)
	return creds, ok
}

func usageLimitResponse(resp *http.Response) (bool, time.Time, error) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests || resp.Body == nil {
		return false, time.Time{}, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, time.Time{}, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	var payload struct {
		Type     string          `json:"type"`
		ResetsAt json.RawMessage `json:"resets_at"`
		Error    struct {
			Type     string          `json:"type"`
			ResetsAt json.RawMessage `json:"resets_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, time.Time{}, nil
	}
	errorType := strings.ToLower(strings.TrimSpace(payload.Error.Type))
	if errorType == "" {
		errorType = strings.ToLower(strings.TrimSpace(payload.Type))
	}
	if errorType != "usage_limit_reached" && errorType != "usage_limit_exceeded" {
		return false, time.Time{}, nil
	}
	resetAt := parseResetAt(payload.Error.ResetsAt)
	if resetAt.IsZero() {
		resetAt = parseResetAt(payload.ResetsAt)
	}
	return true, resetAt, nil
}

func parseResetAt(raw json.RawMessage) time.Time {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || value == "null" {
		return time.Time{}
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		if unix > 10_000_000_000 {
			unix /= 1000
		}
		return time.Unix(unix, 0)
	}
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}
