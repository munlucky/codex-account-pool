package openaiapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/gateway"
)

const (
	testKey           = "gcr_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	testClientVersion = "0.153.4"
)

func TestResponsesRequiresLocalKeyAndRewritesToBackend(t *testing.T) {
	called := 0
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("local client key leaked into backend handler")
		}
		if r.Header.Get("originator") != "codex_cli_rs" || r.Header.Get("version") != testClientVersion {
			t.Fatalf("codex compatibility headers originator=%q version=%q", r.Header.Get("originator"), r.Header.Get("version"))
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["store"] != false || payload["stream"] != true {
			t.Fatalf("store=%v stream=%v body=%s", payload["store"], payload["stream"], body)
		}
		if _, exists := payload["max_output_tokens"]; exists {
			t.Fatalf("max_output_tokens leaked to Codex backend: %s", body)
		}
		input, ok := payload["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("normalized input=%s", body)
		}
		message, _ := input[0].(map[string]any)
		content, _ := message["content"].([]any)
		if message["type"] != "message" || message["role"] != "user" || len(content) != 1 {
			t.Fatalf("normalized input=%s", body)
		}
		span, _ := content[0].(map[string]any)
		if span["type"] != "input_text" || span["text"] != "hi" {
			t.Fatalf("normalized input=%s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","output":[]}`)
	})
	h, err := New(backend, testKey, testClientVersion)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || called != 0 {
		t.Fatalf("status=%d called=%d", rr.Code, called)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi","max_output_tokens":8192}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || called != 1 {
		t.Fatalf("status=%d called=%d body=%s", rr.Code, called, rr.Body.String())
	}
}

func TestResponsesStreamForcesSSEContentType(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["stream"] != true || payload["store"] != false {
			t.Fatalf("upstream contract body=%s", body)
		}
		// ChatGPT Codex can omit Content-Type even though the successful body is SSE.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	h, _ := New(backend, testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"ping","stream":true,"max_output_tokens":8192}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type=%q", got)
	}
	if !strings.Contains(rr.Body.String(), `"response.created"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestResponsesStreamPreservesErrorContentType(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"bad request"}`)
	})
	h, _ := New(backend, testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"ping","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type=%q", got)
	}
}

func TestResponsesNonStreamAggregatesCompletedSSE(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["stream"] != true || payload["store"] != false {
			t.Fatalf("upstream contract body=%s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"status\":\"completed\"}}\n\n")
	})
	h, _ := New(backend, testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"ping","stream":false}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/json" || !strings.Contains(rr.Body.String(), `"text":"pong"`) {
		t.Fatalf("status=%d content-type=%q body=%s", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
}

func TestResponsesRejectsStoreTrue(t *testing.T) {
	called := false
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h, _ := New(backend, testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi","store":true}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || called || !strings.Contains(rr.Body.String(), "store=true") {
		t.Fatalf("status=%d called=%v body=%s", rr.Code, called, rr.Body.String())
	}
}

func TestResponsesPreservesArrayInput(t *testing.T) {
	want := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		var got any
		var expected any
		if err := json.Unmarshal(payload["input"], &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(want), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("input changed: got=%v want=%v", got, expected)
		}
		w.WriteHeader(http.StatusOK)
	})
	h, _ := New(backend, testKey, testClientVersion)
	body := `{"model":"gpt-test","input":` + want + `,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestResponsesRejectsMalformedJSONLocally(t *testing.T) {
	called := false
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h, _ := New(backend, testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || called || !strings.Contains(rr.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("status=%d called=%v body=%s", rr.Code, called, rr.Body.String())
	}
}

func TestModelsNormalizesBackendModelList(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got != testClientVersion {
			t.Fatalf("client_version=%q", got)
		}
		if r.Header.Get("originator") != "codex_cli_rs" || r.Header.Get("version") != testClientVersion {
			t.Fatalf("codex compatibility headers originator=%q version=%q", r.Header.Get("originator"), r.Header.Get("version"))
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-alpha"},{"id":"gpt-beta"}]}`)
	})
	h, _ := New(backend, testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=untrusted", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "gpt-alpha") || !strings.Contains(rr.Body.String(), "gpt-beta") {
		t.Fatalf("models response=%d %s", rr.Code, rr.Body.String())
	}
}

func TestModelsRequiresResolvedCodexClientVersion(t *testing.T) {
	called := false
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h, _ := New(backend, testKey, "")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || called || !strings.Contains(rr.Body.String(), `"type":"configuration_error"`) {
		t.Fatalf("status=%d called=%v body=%s", rr.Code, called, rr.Body.String())
	}
}

func TestModelsConvertsUpstreamHTMLFailureToJSONError(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<html>cloudflare challenge</html>`)
	})
	h, _ := New(backend, testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type=%q", got)
	}
	if strings.Contains(rr.Body.String(), "cloudflare") || !strings.Contains(rr.Body.String(), `"type":"upstream_error"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestModelsRejectsInvalidSuccessfulPayload(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html>unexpected success page</html>`)
	})
	h, _ := New(backend, testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), `"type":"upstream_error"`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestChatCompletionTranslatesRequestAndResponse(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "gpt-test" || payload["stream"] != true || payload["store"] != false || payload["temperature"] != 0.2 {
			t.Fatalf("payload=%s", body)
		}
		tools, _ := payload["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools=%v", payload["tools"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","model":"gpt-test","created_at":123,"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}`)
	})
	h, _ := New(backend, testKey, testClientVersion)
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"temperature":0.2,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	choices := response["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("response=%v", response)
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "hello" || len(message["tool_calls"].([]any)) != 1 {
		t.Fatalf("message=%v", message)
	}
}

func TestChatCompletionStreamingTranslatesTextAndDone(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	})
	h, _ := New(backend, testKey, testClientVersion)
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	scanner := bufio.NewScanner(strings.NewReader(rr.Body.String()))
	var joined string
	for scanner.Scan() {
		joined += scanner.Text() + "\n"
	}
	for _, want := range []string{`"content":"hel"`, `"total_tokens":3`, "data: [DONE]"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
}

func TestChatCompletionRejectsUnknownFields(t *testing.T) {
	h, _ := New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("backend must not be called") }), testKey, testClientVersion)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"n":2}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "n") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

type integrationFailoverProvider struct {
	current  authbroker.Credentials
	fallback authbroker.Credentials
}

func (p *integrationFailoverProvider) Credentials(context.Context) (authbroker.Credentials, error) {
	return p.current, nil
}

func (p *integrationFailoverProvider) Failover(_ context.Context, exhausted string, _ time.Time) (authbroker.Credentials, error) {
	if exhausted == p.fallback.ProfileID || p.fallback.ProfileID == "" {
		return authbroker.Credentials{}, io.EOF
	}
	p.current = p.fallback
	return p.fallback, nil
}

func TestResponsesSurfaceUsesExistingGatewayFailover(t *testing.T) {
	provider := &integrationFailoverProvider{
		current:  authbroker.Credentials{ProfileID: "one", AccessToken: "token-1", AccountID: "acct-1"},
		fallback: authbroker.Credentials{ProfileID: "two", AccessToken: "token-2", AccountID: "acct-2"},
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("upstream path=%s", r.URL.Path)
		}
		switch r.Header.Get("ChatGPT-Account-ID") {
		case "acct-1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_at":2000003600}}`)
		case "acct-2":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_ok","output":[]}`)
		default:
			t.Fatalf("unexpected account=%q", r.Header.Get("ChatGPT-Account-ID"))
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	backend, err := gateway.New(provider, u)
	if err != nil {
		t.Fatal(err)
	}
	var events []gateway.RequestEvent
	backend.SetRequestLogger(func(event gateway.RequestEvent) { events = append(events, event) })
	api, err := New(backend, testKey, testClientVersion)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || calls != 2 || !strings.Contains(rr.Body.String(), "resp_ok") {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, calls, rr.Body.String())
	}
	if len(events) == 0 || events[0].APISurface != "openai_responses" {
		t.Fatalf("api surface not preserved in observability: %+v", events)
	}
}

func TestChatCompletionStreamingTranslatesToolCallDeltas(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_item_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_item_1\",\"output_index\":0,\"delta\":\"{\\\"q\\\":\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_item_1\",\"output_index\":0,\"delta\":\"1}\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":2,\"total_tokens\":6}}}\n\n")
	})
	h, _ := New(backend, testKey, testClientVersion)
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"use tool"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	got := rr.Body.String()
	for _, want := range []string{`"id":"call_1"`, `"name":"lookup"`, "\"arguments\":\"{\\\"q\\\":\"", `"arguments":"1}"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}
