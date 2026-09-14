package openaiapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/munlucky/codex-account-pool/internal/clientauth"
	"github.com/munlucky/codex-account-pool/internal/gateway"
)

type Handler struct {
	backend       http.Handler
	apiKey        string
	clientVersion string
}

func New(backend http.Handler, apiKey, clientVersion string) (*Handler, error) {
	if backend == nil {
		return nil, fmt.Errorf("backend handler is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("client API key is required")
	}
	return &Handler{backend: backend, apiKey: apiKey, clientVersion: strings.TrimSpace(clientVersion)}, nil
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
	clone := h.cloneCodexRequest(r, "/backend-api/codex/responses", "openai_responses")
	requestedStream, err := normalizeResponsesRequest(clone)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	clone.Header.Set("Content-Type", "application/json")
	clone.Header.Del("Content-Encoding")
	clone.Header.Del("Accept-Encoding")

	if requestedStream {
		h.backend.ServeHTTP(&responsesSSEWriter{ResponseWriter: w}, clone)
		return
	}

	capture := newCaptureWriter()
	h.backend.ServeHTTP(capture, clone)
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

func normalizeResponsesRequest(r *http.Request) (bool, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, fmt.Errorf("read request body: %w", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, fmt.Errorf("request body must be valid JSON: %w", err)
	}

	if store, ok := payload["store"]; ok {
		var enabled bool
		if err := json.Unmarshal(store, &enabled); err != nil {
			return false, fmt.Errorf("store must be a boolean")
		}
		if enabled {
			return false, fmt.Errorf("store=true is not supported by the ChatGPT Codex backend")
		}
	}
	payload["store"] = json.RawMessage("false")

	requestedStream := false
	if stream, ok := payload["stream"]; ok {
		if err := json.Unmarshal(stream, &requestedStream); err != nil {
			return false, fmt.Errorf("stream must be a boolean")
		}
	}
	// The ChatGPT Codex backend requires streaming. For non-streaming local
	// callers we aggregate the completed SSE response back into JSON.
	payload["stream"] = json.RawMessage("true")

	// Qwen Code's OpenAI Responses provider always emits max_output_tokens,
	// while the ChatGPT Codex subscription backend rejects that parameter.
	// There is currently no equivalent Codex backend field, so omit it.
	delete(payload, "max_output_tokens")

	if input, ok := payload["input"]; ok {
		var text string
		if err := json.Unmarshal(input, &text); err == nil {
			normalized, err := json.Marshal([]any{map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": text,
				}},
			}})
			if err != nil {
				return false, fmt.Errorf("normalize input: %w", err)
			}
			payload["input"] = normalized
		}
	}

	body, err = json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("encode normalized request: %w", err)
	}
	resetRequestBody(r, body)
	return requestedStream, nil
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
	if h.clientVersion == "" {
		writeError(w, http.StatusServiceUnavailable, "configuration_error", "Codex client version is unavailable. Rerun setup or start Codex once before listing models.")
		return
	}
	clone := h.cloneCodexRequest(r, "/backend-api/codex/models", "openai_models")
	query := clone.URL.Query()
	query.Set("client_version", h.clientVersion)
	clone.URL.RawQuery = query.Encode()
	clone.Header.Del("Accept-Encoding")
	capture := newCaptureWriter()
	h.backend.ServeHTTP(capture, clone)
	if capture.status >= 400 {
		writeError(w, capture.status, "upstream_error", fmt.Sprintf("Codex model catalog returned HTTP %d.", capture.status))
		return
	}
	ids := collectModelIDs(capture.body.Bytes())
	if len(ids) == 0 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Codex model catalog returned an invalid response.")
		return
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "chatgpt-codex"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) cloneCodexRequest(r *http.Request, path, surface string) *http.Request {
	clone := cloneRequest(r, path, surface)
	clone.Header.Set("originator", "codex_cli_rs")
	if h.clientVersion != "" {
		clone.Header.Set("version", h.clientVersion)
	}
	return clone
}

func cloneRequest(r *http.Request, path, surface string) *http.Request {
	ctx := gateway.WithAPISurface(r.Context(), surface)
	clone := r.Clone(ctx)
	clone.URL.Path = path
	clone.URL.RawPath = ""
	clone.Header = r.Header.Clone()
	clone.Header.Del("Authorization")
	return clone
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
