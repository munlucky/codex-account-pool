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

// RouteTarget keeps routing independent of backend transport and credentials.
type RouteTarget struct {
	Provider        string
	Model           string
	AccountSelector string
}
type routeKey struct{}

func RequestRoute(r *http.Request) RouteTarget {
	if r == nil {
		return RouteTarget{}
	}
	target, _ := r.Context().Value(routeKey{}).(RouteTarget)
	return target
}

const AccountSelectorHeader = "X-AI-Account"

type ProviderRouter struct {
	Default   string
	Providers map[string]ResponsesBackend
}

// Resolve preserves bare-model routing to the configured default provider.
func (r *ProviderRouter) Resolve(model string) (ResponsesBackend, string, error) {
	backend, target, err := r.ResolveTarget(model, "")
	return backend, target.Model, err
}
func (r *ProviderRouter) ResolveTarget(model, selector string) (ResponsesBackend, RouteTarget, error) {
	target := RouteTarget{Model: strings.TrimSpace(model), AccountSelector: strings.TrimSpace(selector)}
	if r == nil {
		return nil, target, errors.New("provider router is not configured")
	}
	target.Provider = r.Default
	if target.Provider == "" {
		target.Provider = "codex"
	}
	if strings.Contains(target.Model, "/") {
		target.Provider, target.Model, _ = strings.Cut(target.Model, "/")
	}
	if target.Model == "" || strings.TrimSpace(target.Model) != target.Model || strings.Contains(target.Model, "/") {
		return nil, target, errors.New("invalid model id")
	}
	backend := r.Providers[target.Provider]
	if backend == nil {
		return nil, target, fmt.Errorf("provider %q is not configured", target.Provider)
	}
	if target.AccountSelector != "" {
		capable, ok := backend.(interface{ SupportsAccountSelector() bool })
		if !ok || !capable.SupportsAccountSelector() {
			return nil, target, errors.New("account selector is unsupported by this provider")
		}
		if !validAccountSelector(target.AccountSelector) {
			return nil, target, errors.New("invalid account selector")
		}
	}
	return backend, target, nil
}
func validAccountSelector(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}
func (r *ProviderRouter) Models(ctx context.Context) ([]Model, error) {
	if r == nil {
		return nil, errors.New("provider router is not configured")
	}
	var out []Model
	var errs []error
	names := make([]string, 0, len(r.Providers))
	for name := range r.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	defaultProvider := r.Default
	if defaultProvider == "" {
		defaultProvider = "codex"
	}
	for _, name := range names {
		backend := r.Providers[name]
		if backend == nil {
			continue
		}
		models, err := backend.Models(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		for _, model := range models {
			if name != defaultProvider && !strings.HasPrefix(model.ID, name+"/") {
				model.ID = name + "/" + model.ID
			}
			if model.Object == "" {
				model.Object = "model"
			}
			if model.OwnedBy == "" {
				model.OwnedBy = name
			}
			out = append(out, model)
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
