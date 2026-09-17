package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/munlucky/codex-account-pool/internal/gateway"
)

const googleAntigravityPrefix = "google-antigravity/"

type backendSurfaceKey struct{}

func withBackendSurface(ctx context.Context, surface string) context.Context {
	return context.WithValue(ctx, backendSurfaceKey{}, strings.TrimSpace(surface))
}

func BackendSurface(r *http.Request) string {
	if r == nil {
		return ""
	}
	surface, _ := r.Context().Value(backendSurfaceKey{}).(string)
	return strings.TrimSpace(surface)
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type providerError struct {
	status  int
	typ     string
	message string
}

func (e *providerError) Error() string { return e.message }

func newProviderError(status int, typ, message string) error {
	return &providerError{status: status, typ: typ, message: message}
}

type ResponsesBackend interface {
	ServeResponses(http.ResponseWriter, *http.Request, string)
	Models(context.Context) ([]Model, error)
}

type ProviderRouter struct {
	Codex       ResponsesBackend
	Antigravity ResponsesBackend
}

func (r *ProviderRouter) Resolve(model string) (ResponsesBackend, string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, "", errors.New("model is required")
	}
	if strings.HasPrefix(model, googleAntigravityPrefix) {
		upstream := strings.TrimSpace(strings.TrimPrefix(model, googleAntigravityPrefix))
		if upstream == "" || strings.Contains(upstream, "/") {
			return nil, "", fmt.Errorf("invalid google-antigravity model id %q", model)
		}
		if r == nil || r.Antigravity == nil {
			return nil, "", errors.New("google-antigravity provider is not configured")
		}
		return r.Antigravity, upstream, nil
	}
	if strings.Contains(model, "/") {
		provider, _, _ := strings.Cut(model, "/")
		return nil, "", fmt.Errorf("unsupported provider prefix %q", provider)
	}
	if r == nil || r.Codex == nil {
		return nil, "", errors.New("codex provider is not configured")
	}
	return r.Codex, model, nil
}

func (r *ProviderRouter) Models(ctx context.Context) ([]Model, error) {
	if r == nil {
		return nil, errors.New("provider router is not configured")
	}
	var out []Model
	var errs []error
	if r.Codex != nil {
		models, err := r.Codex.Models(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("codex: %w", err))
		} else {
			out = append(out, models...)
		}
	}
	if r.Antigravity != nil {
		models, err := r.Antigravity.Models(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("google-antigravity: %w", err))
		} else {
			for _, model := range models {
				if !strings.HasPrefix(model.ID, googleAntigravityPrefix) {
					model.ID = googleAntigravityPrefix + model.ID
				}
				if model.Object == "" {
					model.Object = "model"
				}
				if model.OwnedBy == "" {
					model.OwnedBy = "google-antigravity"
				}
				out = append(out, model)
			}
		}
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type CodexBackend struct {
	Handler       http.Handler
	ClientVersion string
}

func NewCodexBackend(handler http.Handler, clientVersion string) *CodexBackend {
	return &CodexBackend{Handler: handler, ClientVersion: strings.TrimSpace(clientVersion)}
}

func (b *CodexBackend) ServeResponses(w http.ResponseWriter, r *http.Request, upstreamModel string) {
	if b == nil || b.Handler == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration_error", "Codex provider is not configured.")
		return
	}
	surface := BackendSurface(r)
	if surface == "" {
		surface = "openai_responses"
	}
	clone := cloneBackendRequest(r, "/backend-api/codex/responses", surface)
	payload, _, err := readResponsesPayload(clone)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	payload["model"] = upstreamModel
	payload["store"] = false
	payload["stream"] = true
	delete(payload, "max_output_tokens")
	body, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Could not encode normalized request.")
		return
	}
	resetRequestBody(clone, body)
	clone.Header.Set("Content-Type", "application/json")
	clone.Header.Del("Content-Encoding")
	clone.Header.Del("Accept-Encoding")
	clone.Header.Set("originator", "codex_cli_rs")
	if b.ClientVersion != "" {
		clone.Header.Set("version", b.ClientVersion)
	}
	b.Handler.ServeHTTP(w, clone)
}

func (b *CodexBackend) Models(ctx context.Context) ([]Model, error) {
	if b == nil || b.Handler == nil {
		return nil, newProviderError(http.StatusServiceUnavailable, "configuration_error", "Codex backend handler is unavailable")
	}
	if b.ClientVersion == "" {
		return nil, newProviderError(http.StatusServiceUnavailable, "configuration_error", "Codex client version is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://router.local/backend-api/codex/models", nil)
	if err != nil {
		return nil, err
	}
	req = cloneBackendRequest(req, "/backend-api/codex/models", "openai_models")
	query := req.URL.Query()
	query.Set("client_version", b.ClientVersion)
	req.URL.RawQuery = query.Encode()
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("version", b.ClientVersion)
	req.Header.Del("Accept-Encoding")
	capture := newCaptureWriter()
	b.Handler.ServeHTTP(capture, req)
	if capture.status >= 400 {
		return nil, newProviderError(capture.status, "upstream_error", fmt.Sprintf("Codex model catalog returned HTTP %d", capture.status))
	}
	ids := collectModelIDs(capture.body.Bytes())
	if len(ids) == 0 {
		return nil, newProviderError(http.StatusBadGateway, "upstream_error", "Codex model catalog returned an invalid response")
	}
	models := make([]Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, Model{ID: id, Object: "model", OwnedBy: "chatgpt-codex"})
	}
	return models, nil
}

func cloneBackendRequest(r *http.Request, path, surface string) *http.Request {
	ctx := gateway.WithAPISurface(r.Context(), surface)
	clone := r.Clone(ctx)
	clone.URL.Path = path
	clone.URL.RawPath = ""
	clone.Header = r.Header.Clone()
	clone.Header.Del("Authorization")
	return clone
}
