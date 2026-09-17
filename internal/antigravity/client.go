package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/munlucky/codex-account-pool/internal/antigravityauth"
	"github.com/munlucky/codex-account-pool/internal/observability"
	"github.com/munlucky/codex-account-pool/internal/openaiapi"
)

const defaultBaseURL = "https://daily-cloudcode-pa.googleapis.com"

type CredentialProvider interface {
	Credentials(context.Context) (antigravityauth.Credentials, error)
	ForceRefresh(context.Context, string) (antigravityauth.Credentials, error)
}

type failoverCredentialProvider interface {
	CandidateCredentials(context.Context, string) []antigravityauth.Credentials
}

type cachedCatalog struct {
	models    []openaiapi.Model
	expiresAt time.Time
}

type Client struct {
	Broker    CredentialProvider
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
	Replay    *ReplayCache
	Logger    func(observability.Event)
	Now       func() time.Time

	catalogMu sync.Mutex
	catalog   map[string]cachedCatalog
}

func New(broker CredentialProvider) *Client {
	httpClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Client{
		Broker: broker, HTTP: httpClient, BaseURL: defaultBaseURL, UserAgent: antigravityauth.UserAgent(), Replay: NewReplayCache(0, 0), Now: time.Now,
		catalog: make(map[string]cachedCatalog),
	}
}

func (c *Client) SetRequestLogger(logger func(observability.Event)) { c.Logger = logger }

func (c *Client) ServeResponses(w http.ResponseWriter, r *http.Request, upstreamModel string) {
	requestID := "ag_" + randomHex(10)
	started := c.now()
	surface := openaiapi.BackendSurface(r)
	if surface == "" {
		surface = "openai_responses"
	}
	c.emit(observability.Event{Timestamp: started, SchemaVersion: observability.SchemaVersion, EventType: observability.EventRequestStart, RequestID: requestID, Method: http.MethodPost, RouteTemplate: "/v1internal:streamGenerateContent", APISurface: surface, Provider: "google-antigravity", Transport: "http"})

	payload, err := decodeResponsesPayload(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		c.end(requestID, surface, started, http.StatusBadRequest, "local", observability.OutcomeLocalResponse, observability.SemanticFailed)
		return
	}
	credentials, err := c.Broker.Credentials(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "provider_not_configured", "Google Antigravity login is required. Run `gpt-codex-router auth add google-antigravity <profile>`. ")
		c.end(requestID, surface, started, http.StatusServiceUnavailable, "local", observability.OutcomeLocalResponse, observability.SemanticFailed)
		return
	}
	session := sessionID(r, payload)
	prepared, err := prepareRequest(payload, upstreamModel, session, credentials.ProfileID, c.replay())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		c.end(requestID, surface, started, http.StatusBadRequest, "local", observability.OutcomeLocalResponse, observability.SemanticFailed)
		return
	}

	attempt := 1
	resp, elapsed, err := c.sendInference(r.Context(), credentials, prepared)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "upstream_error", "Google Antigravity upstream request failed.")
		c.end(requestID, surface, started, http.StatusBadGateway, "local", observability.OutcomeLocalResponse, observability.SemanticFailed)
		return
	}
	c.emit(observability.Event{Timestamp: c.now(), SchemaVersion: observability.SchemaVersion, EventType: observability.EventUpstreamAttempt, RequestID: requestID, RouteTemplate: "/v1internal:streamGenerateContent", APISurface: surface, Provider: "google-antigravity", Transport: "http", Attempt: attempt, StatusCode: resp.StatusCode, StatusOrigin: "upstream", UpstreamHeadersMS: float64(elapsed.Microseconds()) / 1000})

	if resp.StatusCode == http.StatusUnauthorized {
		drainAndClose(resp.Body)
		refreshed, refreshErr := c.Broker.ForceRefresh(r.Context(), credentials.ProfileID)
		if refreshErr == nil {
			credentials = refreshed
			attempt++
			resp, elapsed, err = c.sendInference(r.Context(), credentials, prepared)
			if err == nil {
				c.emit(observability.Event{Timestamp: c.now(), SchemaVersion: observability.SchemaVersion, EventType: observability.EventUpstreamAttempt, RequestID: requestID, RouteTemplate: "/v1internal:streamGenerateContent", APISurface: surface, Provider: "google-antigravity", Transport: "http", Attempt: attempt, StatusCode: resp.StatusCode, StatusOrigin: "upstream", UpstreamHeadersMS: float64(elapsed.Microseconds()) / 1000})
			}
		}
		if refreshErr != nil || err != nil {
			writeAPIError(w, http.StatusUnauthorized, "authentication_error", "Google Antigravity authentication expired. Re-run the provider login command.")
			c.end(requestID, surface, started, http.StatusUnauthorized, "local", observability.OutcomeLocalResponse, observability.SemanticFailed)
			return
		}
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		if pool, ok := c.Broker.(failoverCredentialProvider); ok {
			probeCtx, cancel := quotaProbeContext(r.Context())
			currentQuota := c.probeModelQuota(probeCtx, credentials, prepared.WireModel)
			cancel()
			if currentQuota == quotaExhausted {
				candidates := pool.CandidateCredentials(r.Context(), credentials.ProfileID)
				for _, candidate := range candidates {
					probeCtx, cancel := quotaProbeContext(r.Context())
					candidateQuota := c.probeModelQuota(probeCtx, candidate, prepared.WireModel)
					cancel()
					if candidateQuota != quotaUsable {
						continue
					}
					candidatePrepared, prepareErr := prepareRequest(payload, upstreamModel, session, candidate.ProfileID, c.replay())
					if prepareErr != nil {
						continue
					}
					drainAndClose(resp.Body)
					credentials = candidate
					prepared = candidatePrepared
					attempt++
					resp, elapsed, err = c.sendInference(r.Context(), credentials, prepared)
					if err != nil {
						writeAPIError(w, http.StatusBadGateway, "upstream_error", "Google Antigravity failover request failed.")
						c.end(requestID, surface, started, http.StatusBadGateway, "local", observability.OutcomeLocalResponse, observability.SemanticFailed)
						return
					}
					c.emit(observability.Event{Timestamp: c.now(), SchemaVersion: observability.SchemaVersion, EventType: observability.EventUpstreamAttempt, RequestID: requestID, RouteTemplate: "/v1internal:streamGenerateContent", APISurface: surface, Provider: "google-antigravity", Transport: "http", Attempt: attempt, StatusCode: resp.StatusCode, StatusOrigin: "upstream", UpstreamHeadersMS: float64(elapsed.Microseconds()) / 1000})
					break
				}
			}
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		hint := readErrorHint(resp.Body)
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(hint, "signature") {
			c.replay().ClearSession(credentials.ProfileID, prepared.WireModel, session)
		}
		status := resp.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		message := fmt.Sprintf("Google Antigravity upstream returned HTTP %d.", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized {
			message = "Google Antigravity authentication was rejected. Re-run the provider login command."
		}
		writeAPIError(w, status, "upstream_error", message)
		c.end(requestID, surface, started, status, "upstream", observability.OutcomeBodyEOF, observability.SemanticFailed)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	externalModel := "google-antigravity/" + upstreamModel
	translateErr := translateCCAStream(w, resp.Body, externalModel, prepared.WireModel, credentials.ProfileID, session, c.replay())
	semantic := observability.SemanticCompleted
	outcome := observability.OutcomeBodyEOF
	if translateErr != nil {
		semantic = observability.SemanticFailed
		outcome = observability.OutcomeUpstreamReadError
	}
	c.end(requestID, surface, started, http.StatusOK, "upstream", outcome, semantic)
}

func (c *Client) Models(ctx context.Context) ([]openaiapi.Model, error) {
	credentials, err := c.Broker.Credentials(ctx)
	if err != nil {
		return staticModels(), nil
	}
	now := c.now()
	c.catalogMu.Lock()
	cached, ok := c.catalog[credentials.ProfileID]
	c.catalogMu.Unlock()
	if ok && cached.expiresAt.After(now) {
		return append([]openaiapi.Model(nil), cached.models...), nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	models, status, err := c.fetchModels(fetchCtx, credentials)
	if status == http.StatusUnauthorized && err == nil {
		refreshed, refreshErr := c.Broker.ForceRefresh(fetchCtx, credentials.ProfileID)
		if refreshErr == nil {
			credentials = refreshed
			models, _, err = c.fetchModels(fetchCtx, credentials)
		}
	}
	if err != nil || len(models) == 0 {
		return staticModels(), nil
	}
	c.catalogMu.Lock()
	c.catalog[credentials.ProfileID] = cachedCatalog{models: append([]openaiapi.Model(nil), models...), expiresAt: now.Add(10 * time.Minute)}
	c.catalogMu.Unlock()
	return models, nil
}

func (c *Client) sendInference(ctx context.Context, credentials antigravityauth.Credentials, prepared preparedRequest) (*http.Response, time.Duration, error) {
	envelope := map[string]any{
		"model":       prepared.WireModel,
		"userAgent":   "antigravity",
		"requestType": "agent",
		"project":     credentials.ProjectID,
		"requestId":   "agent-" + randomHex(16),
		"request":     prepared.Body,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, 0, err
	}
	endpoint := strings.TrimRight(c.baseURL(), "/") + "/v1internal:streamGenerateContent?alt=sse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	c.setHeaders(req, credentials.AccessToken)
	started := time.Now()
	resp, err := c.httpClient().Do(req)
	return resp, time.Since(started), err
}

func (c *Client) fetchModels(ctx context.Context, credentials antigravityauth.Credentials) ([]openaiapi.Model, int, error) {
	body, _ := json.Marshal(map[string]any{"project": credentials.ProjectID})
	endpoint := strings.TrimRight(c.baseURL(), "/") + "/v1internal:fetchAvailableModels"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	c.setHeaders(req, credentials.AccessToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		drainAndClose(resp.Body)
		return nil, resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, resp.StatusCode, fmt.Errorf("model discovery HTTP %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, resp.StatusCode, errors.New("invalid Antigravity model discovery response")
	}
	return parseAvailableModels(payload), resp.StatusCode, nil
}

func decodeResponsesPayload(body io.Reader) (map[string]any, error) {
	decoder := json.NewDecoder(io.LimitReader(body, 16<<20))
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, errors.New("request body must be valid JSON")
	}
	if payload == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	return payload, nil
}

func (c *Client) setHeaders(req *http.Request, accessToken string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
}

func (c *Client) emit(event observability.Event) {
	if c.Logger != nil {
		c.Logger(event)
	}
}

func (c *Client) end(requestID, surface string, started time.Time, status int, origin, outcome, semantic string) {
	c.emit(observability.Event{
		Timestamp: c.now(), SchemaVersion: observability.SchemaVersion, EventType: observability.EventRequestEnd,
		RequestID: requestID, Method: http.MethodPost, RouteTemplate: "/v1internal:streamGenerateContent", APISurface: surface,
		Provider: "google-antigravity", Transport: "http", StatusCode: status, StatusOrigin: origin, Outcome: outcome,
		SemanticOutcome: semantic, GatewayTotalMS: float64(c.now().Sub(started).Microseconds()) / 1000,
	})
}

func (c *Client) replay() *ReplayCache {
	if c.Replay == nil {
		c.Replay = NewReplayCache(0, 0)
	}
	return c.Replay
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) baseURL() string {
	if strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimSpace(c.BaseURL)
	}
	return defaultBaseURL
}

func (c *Client) userAgent() string {
	if strings.TrimSpace(c.UserAgent) != "" {
		return strings.TrimSpace(c.UserAgent)
	}
	return antigravityauth.UserAgent()
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func readErrorHint(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, 16<<10))
	return strings.ToLower(string(data))
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

func writeAPIError(w http.ResponseWriter, status int, typ, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": typ}})
}
