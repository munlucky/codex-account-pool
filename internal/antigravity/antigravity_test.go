package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/antigravityauth"
)

const testSignature = "AbCdEfGhIjKlMnOpQrStUvWxYz012345"

func TestPrepareRequestReplaysThoughtSignature(t *testing.T) {
	replay := NewReplayCache(16, time.Minute)
	args := map[string]any{"city": "Seoul"}
	replay.Remember("google-1", "gemini-3.8-flash-medium", "-123", "weather", args, testSignature)
	payload := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "weather?"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "weather", "arguments": `{"city":"Seoul"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
		},
		"tools":     []any{map[string]any{"type": "function", "name": "weather", "parameters": map[string]any{"type": "object"}}},
		"reasoning": map[string]any{"effort": "medium"},
	}
	prepared, err := prepareRequest(payload, "gemini-3.8-flash", "-123", "google-1", replay)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.WireModel != "gemini-3.8-flash-medium" {
		t.Fatalf("wire model=%q", prepared.WireModel)
	}
	contents := prepared.Body["contents"].([]any)
	callContent := contents[1].(map[string]any)
	part := callContent["parts"].([]any)[0].(map[string]any)
	if part["thoughtSignature"] != testSignature {
		t.Fatalf("thoughtSignature=%v", part["thoughtSignature"])
	}
	resultContent := contents[2].(map[string]any)
	responsePart := resultContent["parts"].([]any)[0].(map[string]any)
	response := responsePart["functionResponse"].(map[string]any)
	if response["id"] != "call_1" || response["name"] != "weather" {
		t.Fatalf("functionResponse=%v", response)
	}
}

func TestTranslateCCAStreamEmitsCanonicalResponsesAndCachesSignature(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"hello "},{"functionCall":{"id":"call_1","name":"weather","args":{"city":"Seoul"}},"thoughtSignature":"` + testSignature + `"}]}}]}}`,
		"",
		`data: {"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"world"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":1,"totalTokenCount":6}}}`,
		"",
	}, "\n")
	replay := NewReplayCache(16, time.Minute)
	recorder := httptest.NewRecorder()
	if err := translateCCAStream(recorder, strings.NewReader(stream), "google-antigravity/gemini-3.8-flash", "gemini-3.8-flash-medium", "google-1", "-123", replay); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, eventType := range []string{"response.output_text.delta", "response.function_call_arguments.delta", "response.output_item.done", "response.completed"} {
		if !strings.Contains(body, eventType) {
			t.Fatalf("missing %s in %s", eventType, body)
		}
	}
	if !strings.Contains(body, `"reasoning_tokens":1`) {
		t.Fatalf("missing reasoning usage: %s", body)
	}
	if signature, ok := replay.Lookup("google-1", "gemini-3.8-flash-medium", "-123", "weather", map[string]any{"city": "Seoul"}); !ok || signature != testSignature {
		t.Fatalf("cached signature=%q ok=%v", signature, ok)
	}
}

func TestParseAvailableModelsCollapsesEffortWireIDs(t *testing.T) {
	payload := map[string]any{
		"models": map[string]any{
			"gemini-3.8-flash-low": map[string]any{}, "gemini-3.8-flash-medium": map[string]any{}, "gemini-3.8-flash-high": map[string]any{},
			"claude-sonnet-4-6": map[string]any{},
		},
		"agentModelSorts": []any{map[string]any{"groups": []any{map[string]any{"modelIds": []any{"gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.8-flash-high", "claude-sonnet-4-6"}}}}},
	}
	models := parseAvailableModels(payload)
	got := map[string]bool{}
	for _, model := range models {
		got[model.ID] = true
	}
	if !got["gemini-3.8-flash"] || !got["claude-sonnet-4-6"] || got["gemini-3.8-flash-low"] {
		t.Fatalf("models=%v", models)
	}
}

type fakeBroker struct {
	current      antigravityauth.Credentials
	refreshed    antigravityauth.Credentials
	refreshCalls int
}

func (b *fakeBroker) Credentials(context.Context) (antigravityauth.Credentials, error) {
	return b.current, nil
}
func (b *fakeBroker) ForceRefresh(context.Context, string) (antigravityauth.Credentials, error) {
	b.refreshCalls++
	return b.refreshed, nil
}

func TestClientRefreshesSameAccountOnceOn401(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.Header.Get("Authorization") != "Bearer old" {
				t.Fatalf("first authorization=%q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new" {
			t.Fatalf("second authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"pong\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}}\n\n")
	}))
	defer server.Close()
	broker := &fakeBroker{
		current:   antigravityauth.Credentials{ProfileID: "google-1", AccessToken: "old", ProjectID: "project-1"},
		refreshed: antigravityauth.Credentials{ProfileID: "google-1", AccessToken: "new", ProjectID: "project-1"},
	}
	client := New(broker)
	client.BaseURL = server.URL
	client.HTTP = server.Client()
	body := `{"model":"google-antigravity/gemini-3.8-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}],"store":false,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	client.ServeResponses(recorder, req, "gemini-3.8-flash")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "pong") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if broker.refreshCalls != 1 || requests != 2 {
		t.Fatalf("refresh=%d requests=%d", broker.refreshCalls, requests)
	}
}

func TestClientDoesNotRotateOrRefreshOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer server.Close()
	broker := &fakeBroker{current: antigravityauth.Credentials{ProfileID: "google-1", AccessToken: "old", ProjectID: "project-1"}}
	client := New(broker)
	client.BaseURL = server.URL
	client.HTTP = server.Client()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"input":[{"type":"message","role":"user","content":"ping"}]}`))
	recorder := httptest.NewRecorder()
	client.ServeResponses(recorder, req, "gemini-3.8-flash")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if broker.refreshCalls != 0 {
		t.Fatalf("unexpected refresh calls=%d", broker.refreshCalls)
	}
}

func TestModelsUsesStaticFallbackWhenProfileUnavailable(t *testing.T) {
	broker := &errorBroker{}
	models, err := New(broker).Models(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("models=%v err=%v", models, err)
	}
}

type errorBroker struct{}

func (*errorBroker) Credentials(context.Context) (antigravityauth.Credentials, error) {
	return antigravityauth.Credentials{}, io.EOF
}
func (*errorBroker) ForceRefresh(context.Context, string) (antigravityauth.Credentials, error) {
	return antigravityauth.Credentials{}, io.EOF
}

func TestEnvelopeDoesNotMixTokenAndProjectAcrossRefresh(t *testing.T) {
	var envelopes []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		envelopes = append(envelopes, body)
		if len(envelopes) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"ok\"}]}}],\"usageMetadata\":{\"totalTokenCount\":1}}}\n\n")
	}))
	defer server.Close()
	broker := &fakeBroker{
		current:   antigravityauth.Credentials{ProfileID: "google-1", AccessToken: "old", ProjectID: "project-A"},
		refreshed: antigravityauth.Credentials{ProfileID: "google-1", AccessToken: "new", ProjectID: "project-B"},
	}
	client := New(broker)
	client.BaseURL = server.URL
	client.HTTP = server.Client()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":[{"type":"message","role":"user","content":"x"}]}`))
	client.ServeResponses(httptest.NewRecorder(), req, "gemini-3.8-flash")
	if len(envelopes) != 2 || envelopes[0]["project"] != "project-A" || envelopes[1]["project"] != "project-B" {
		t.Fatalf("envelopes=%v", envelopes)
	}
}

type fakePoolBroker struct {
	fakeBroker
	candidates []antigravityauth.Credentials
}

func (b *fakePoolBroker) CandidateCredentials(context.Context, string) []antigravityauth.Credentials {
	return append([]antigravityauth.Credentials(nil), b.candidates...)
}

func TestClientFailsOverOnlyAfterVerifiedQuotaExhaustion(t *testing.T) {
	streamAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			remaining := 1.0
			if request["project"] == "project-A" {
				remaining = 0
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-3.8-flash-medium": map[string]any{
						"quotaInfo": map[string]any{"remainingFraction": remaining},
					},
				},
			})
		case "/v1internal:streamGenerateContent":
			streamAttempts++
			if streamAttempts == 1 {
				if r.Header.Get("Authorization") != "Bearer token-A" {
					t.Fatalf("first token=%q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if r.Header.Get("Authorization") != "Bearer token-B" {
				t.Fatalf("failover token=%q", r.Header.Get("Authorization"))
			}
			var envelope map[string]any
			_ = json.NewDecoder(r.Body).Decode(&envelope)
			if envelope["project"] != "project-B" {
				t.Fatalf("failover project=%v", envelope["project"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"pong\"}]}}],\"usageMetadata\":{\"totalTokenCount\":1}}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	broker := &fakePoolBroker{
		fakeBroker: fakeBroker{current: antigravityauth.Credentials{ProfileID: "google-1", AccessToken: "token-A", ProjectID: "project-A"}},
		candidates: []antigravityauth.Credentials{{ProfileID: "google-2", AccessToken: "token-B", ProjectID: "project-B"}},
	}
	client := New(broker)
	client.BaseURL = server.URL
	client.HTTP = server.Client()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":[{"type":"message","role":"user","content":"ping"}]}`))
	recorder := httptest.NewRecorder()
	client.ServeResponses(recorder, req, "gemini-3.8-flash")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "pong") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if streamAttempts != 2 {
		t.Fatalf("stream attempts=%d", streamAttempts)
	}
}

func TestClientDoesNotFailOverWhen429QuotaStillUsable(t *testing.T) {
	streamAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-3.8-flash-medium": map[string]any{
						"quotaInfo": map[string]any{"remainingFraction": 0.5},
					},
				},
			})
		case "/v1internal:streamGenerateContent":
			streamAttempts++
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	broker := &fakePoolBroker{
		fakeBroker: fakeBroker{current: antigravityauth.Credentials{ProfileID: "google-1", AccessToken: "token-A", ProjectID: "project-A"}},
		candidates: []antigravityauth.Credentials{{ProfileID: "google-2", AccessToken: "token-B", ProjectID: "project-B"}},
	}
	client := New(broker)
	client.BaseURL = server.URL
	client.HTTP = server.Client()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":[{"type":"message","role":"user","content":"ping"}]}`))
	recorder := httptest.NewRecorder()
	client.ServeResponses(recorder, req, "gemini-3.8-flash")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if streamAttempts != 1 {
		t.Fatalf("stream attempts=%d", streamAttempts)
	}
}
