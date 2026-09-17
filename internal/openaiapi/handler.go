package openaiapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/munlucky/codex-account-pool/internal/clientauth"
)

type Handler struct {
	router *ProviderRouter
	apiKey string
}

func New(backend http.Handler, apiKey, clientVersion string) (*Handler, error) {
	if backend == nil {
		return nil, fmt.Errorf("backend handler is required")
	}
	return NewWithRouter(&ProviderRouter{Codex: NewCodexBackend(backend, clientVersion)}, apiKey)
}

func NewWithRouter(router *ProviderRouter, apiKey string) (*Handler, error) {
	if router == nil {
		return nil, fmt.Errorf("provider router is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("client API key is required")
	}
	return &Handler{router: router, apiKey: apiKey}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !clientauth.Matches(r.Header.Get("Authorization"), h.apiKey) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid local router API key.")
		return
	}
	switch r.URL.Path {
	case "/v1/responses":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.forwardResponses(w, r)
	case "/v1/models":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.models(w, r)
	case "/v1/chat/completions":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.chatCompletions(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) forwardResponses(w http.ResponseWriter, r *http.Request) {
	payload, requestedStream, err := readResponsesPayload(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model, _ := payload["model"].(string)
	backend, upstreamModel, err := h.router.Resolve(model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Could not encode normalized request.")
		return
	}
	clone := r.Clone(withBackendSurface(r.Context(), "openai_responses"))
	clone.Header = r.Header.Clone()
	clone.Header.Del("Authorization")
	resetRequestBody(clone, body)
	clone.Header.Set("Content-Type", "application/json")
	clone.Header.Del("Content-Encoding")
	clone.Header.Del("Accept-Encoding")

	if requestedStream {
		backend.ServeResponses(&responsesSSEWriter{ResponseWriter: w}, clone, upstreamModel)
		return
	}

	capture := newCaptureWriter()
	backend.ServeResponses(capture, clone, upstreamModel)
	if capture.status >= 400 {
		copyCaptured(w, capture)
		return
	}
	response, err := completedResponsesPayload(capture.body.Bytes(), capture.header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("Could not assemble upstream Responses payload: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

type responsesSSEWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *responsesSSEWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if statusCode >= 200 && statusCode < 300 {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responsesSSEWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responsesSSEWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func readResponsesPayload(r *http.Request) (map[string]any, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return nil, false, fmt.Errorf("read request body: %w", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("request body must be valid JSON: %w", err)
	}
	if payload == nil {
		return nil, false, fmt.Errorf("request body must be a JSON object")
	}

	if store, ok := payload["store"]; ok {
		enabled, ok := store.(bool)
		if !ok {
			return nil, false, fmt.Errorf("store must be a boolean")
		}
		if enabled {
			return nil, false, fmt.Errorf("store=true is not supported by the local router")
		}
	}
	payload["store"] = false

	requestedStream := false
	if stream, ok := payload["stream"]; ok {
		value, ok := stream.(bool)
		if !ok {
			return nil, false, fmt.Errorf("stream must be a boolean")
		}
		requestedStream = value
	}

	model, ok := payload["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return nil, false, fmt.Errorf("model is required")
	}
	payload["model"] = strings.TrimSpace(model)

	if input, ok := payload["input"].(string); ok {
		payload["input"] = []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": input,
			}},
		}}
	}
	return payload, requestedStream, nil
}

func completedResponsesPayload(body []byte, contentType string) (map[string]any, error) {
	var response map[string]any
	if json.Unmarshal(body, &response) == nil && response != nil {
		return response, nil
	}
	if response = completedResponseFromSSE(body); response != nil {
		return response, nil
	}
	return nil, fmt.Errorf("unrecognized upstream Responses payload: content-type=%q bytes=%d", contentType, len(body))
}

func resetRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.Header.Del("Content-Length")
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	models, err := h.router.Models(r.Context())
	if err != nil {
		var providerErr *providerError
		if errors.As(err, &providerErr) {
			message := "Provider model catalog request failed."
			if providerErr.typ == "configuration_error" {
				message = "Codex client version is unavailable. Rerun setup or start Codex once before listing models."
			}
			writeError(w, providerErr.status, providerErr.typ, message)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", "No provider model catalog is currently available.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: make(http.Header), status: http.StatusOK}
}
func (w *captureWriter) Header() http.Header         { return w.header }
func (w *captureWriter) WriteHeader(code int)        { w.status = code }
func (w *captureWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func copyCaptured(w http.ResponseWriter, captured *captureWriter) {
	for k, values := range captured.header {
		for _, value := range values {
			w.Header().Add(k, value)
		}
	}
	w.WriteHeader(captured.status)
	_, _ = w.Write(captured.body.Bytes())
}

func collectModelIDs(body []byte) []string {
	var root any
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			for _, key := range []string{"id", "slug", "model"} {
				if s, ok := v[key].(string); ok && looksLikeModelID(s) {
					seen[s] = struct{}{}
				}
			}
			for _, key := range []string{"data", "models", "items"} {
				if child, ok := v[key]; ok {
					walk(child)
				}
			}
		}
	}
	walk(root)
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func looksLikeModelID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, " \t\r\n")
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed.")
}

func writeError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": typ}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
