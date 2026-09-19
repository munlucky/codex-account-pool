package antigravity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/antigravityauth"
	"github.com/munlucky/codex-account-pool/internal/observability"
	"github.com/munlucky/codex-account-pool/internal/openaiapi"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

const toolStream = "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"functionCall\":{\"id\":\"call_1\",\"name\":\"weather\",\"args\":{\"city\":\"Seoul\"}},\"thoughtSignature\":\"" + testSignature + "\"}]}}]}}\n\n"
const textStream = "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"sunny\"}]}}]}}\n\n"
const firstPayload = `{"model":"google-antigravity/gemini-3.8-flash","input":[{"type":"message","role":"user","content":"weather?"}]}`
const nextPayload = `{"model":"google-antigravity/gemini-3.8-flash","input":[{"type":"message","role":"user","content":"weather?"},{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Seoul\"}"},{"type":"function_call_output","call_id":"call_1","output":"sunny"}]}`

func testRegistry(t *testing.T) (*profile.Store, *antigravityauth.Broker) {
	t.Helper()
	store := profile.NewStore(t.TempDir())
	registry, _ := store.Load()
	for _, id := range []string{"A", "B"} {
		if err := registry.Add(profile.Profile{ID: id, Provider: profile.ProviderGoogleAntigravity, Isolation: "oauth-state"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}
	broker := antigravityauth.NewBroker(store)
	for _, id := range []string{"A", "B"} {
		if err := broker.CredentialsStore.Save(id, antigravityauth.Credential{AccessToken: "token-" + id, RefreshToken: "refresh-" + id, ProjectID: "project-" + id, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	return store, broker
}
func testAPI(t *testing.T, c *Client) *openaiapi.Handler {
	t.Helper()
	h, err := openaiapi.NewWithRouter(&openaiapi.ProviderRouter{Default: "codex", Providers: map[string]openaiapi.ResponsesBackend{"google-antigravity": c}}, "local-test")
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func apiCall(h http.Handler, path, payload, session, account string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	r.Header.Set("Authorization", "Bearer local-test")
	if session != "" {
		r.Header.Set("X-Client-Thread-Id", session)
	}
	if account != "" {
		r.Header.Set(openaiapi.AccountSelectorHeader, account)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func returnedCallID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var response struct {
		Output []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, item := range response.Output {
		if item.Type == "function_call" {
			return item.CallID
		}
	}
	t.Fatal("missing function call", w.Body.String())
	return ""
}

func TestAPIFailoverPreservesNextTurnSignatureAndFreshCredentials(t *testing.T) {
	_, broker := testRegistry(t)
	var mu sync.Mutex
	attempts := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(openaiapi.AccountSelectorHeader) != "" {
			t.Error("local selector leaked upstream")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(r.URL.Path, "fetchAvailableModels") {
			remaining := 1.0
			if body["project"] == "project-A" {
				remaining = 0
			}
			json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{"gemini-3.8-flash-medium": map[string]any{"quotaInfo": map[string]any{"remainingFraction": remaining}}}})
			return
		}
		mu.Lock()
		attempts = append(attempts, r.Header.Get("Authorization"))
		attempt := len(attempts)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(429)
			return
		}
		if body["project"] != "project-B" {
			t.Error("wrong project", body["project"])
		}
		if attempt == 2 {
			io.WriteString(w, toolStream)
			return
		}
		encoded, _ := json.Marshal(body)
		if !strings.Contains(string(encoded), testSignature) {
			t.Error("missing replay signature")
		}
		io.WriteString(w, textStream)
	}))
	defer server.Close()
	c := New(broker)
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	h := testAPI(t, c)
	w := apiCall(h, "/v1/responses", firstPayload, "s1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "function_call") {
		t.Fatal(w.Code, w.Body.String())
	}
	callID := returnedCallID(t, w)
	// The pool retains only an ID; each turn loads a fresh token/project snapshot.
	bCredential, _ := broker.CredentialsStore.Load("B")
	bCredential.AccessToken = "fresh-B"
	if err := broker.CredentialsStore.Save("B", bCredential); err != nil {
		t.Fatal(err)
	}
	if w := apiCall(h, "/v1/responses", strings.ReplaceAll(nextPayload, "call_1", callID), "s1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "sunny") {
		t.Fatal(w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(attempts, ",") != "Bearer token-A,Bearer token-B,Bearer fresh-B" {
		t.Fatal(attempts)
	}
}

func TestHeaderlessToolHandlesPreserveIndependentConversations(t *testing.T) {
	store, broker := testRegistry(t)
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "functionResponse") {
			if !strings.Contains(string(body), testSignature) || !strings.Contains(string(body), `"id":"call_1"`) {
				t.Error("wire id/signature not restored", string(body))
			}
			io.WriteString(w, textStream)
		} else {
			io.WriteString(w, toolStream)
		}
	}))
	defer server.Close()
	c := New(broker)
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	h := testAPI(t, c)
	first := apiCall(h, "/v1/responses", firstPayload, "", "")
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	a := returnedCallID(t, first)
	r, _ := store.Load()
	r.Use(profile.ProviderGoogleAntigravity, "B")
	store.Save(r)
	second := apiCall(h, "/v1/responses", firstPayload, "", "")
	if second.Code != 200 {
		t.Fatal(second.Body.String())
	}
	b := returnedCallID(t, second)
	if a == b {
		t.Fatal("different conversations share call handle")
	}
	for _, id := range []string{a, b} {
		w := apiCall(h, "/v1/responses", strings.ReplaceAll(nextPayload, "call_1", id), "", "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if strings.Join(tokens, ",") != "Bearer token-A,Bearer token-B,Bearer token-A,Bearer token-B" {
		t.Fatal(tokens)
	}
	tampered := strings.ReplaceAll(strings.ReplaceAll(nextPayload, "call_1", a), "Seoul", "Busan")
	if w := apiCall(h, "/v1/responses", tampered, "", ""); w.Code != 503 {
		t.Fatal("tampered handle accepted", w.Code)
	}
	if len(tokens) != 4 {
		t.Fatal("tampering reached upstream")
	}
}

func TestAPIAuthUseAndExplicitAccountSelection(t *testing.T) {
	store, broker := testRegistry(t)
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.Header.Get(openaiapi.AccountSelectorHeader) != "" {
			t.Error("selector leaked")
		}
		io.WriteString(w, textStream)
	}))
	defer server.Close()
	c := New(broker)
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	h := testAPI(t, c)
	check := func(session, selector string) {
		t.Helper()
		w := apiCall(h, "/v1/responses", firstPayload, session, selector)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	check("old", "")
	registry, _ := store.Load()
	registry.Use(profile.ProviderGoogleAntigravity, "B")
	store.Save(registry)
	check("old", "")
	check("new", "")
	check("explicit", "A")
	if strings.Join(tokens, ",") != "Bearer token-A,Bearer token-A,Bearer token-B,Bearer token-A" {
		t.Fatal(tokens)
	}
	w := apiCall(h, "/v1/responses", firstPayload, "missing", "no-such-profile")
	if w.Code != 503 || len(tokens) != 4 {
		t.Fatal(w.Code, tokens)
	}
}

func TestAPIContinuityLossFailsBeforeUpstream(t *testing.T) {
	_, broker := testRegistry(t)
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts++; io.WriteString(w, toolStream) }))
	defer server.Close()
	c := New(broker)
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	h := testAPI(t, c)
	if w := apiCall(h, "/v1/responses", firstPayload, "one", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, session := range []string{"different", ""} {
		w := apiCall(h, "/v1/responses", nextPayload, session, "")
		if w.Code != 503 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := apiCall(h, "/v1/responses", nextPayload, "one", "B")
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	c.Replay.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	w = apiCall(h, "/v1/responses", nextPayload, "one", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "session_continuity_unavailable") {
		t.Fatal(w.Code, w.Body.String())
	}
	if attempts != 1 {
		t.Fatal("unsafe replay reached upstream", attempts)
	}
	// A process restart loses both bounded caches and must not synthesize history.
	restarted := New(broker)
	restarted.BaseURL = server.URL
	w = apiCall(testAPI(t, restarted), "/v1/responses", nextPayload, "one", "")
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestChatAPIPreservesToolContinuity(t *testing.T) {
	_, broker := testRegistry(t)
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			io.WriteString(w, toolStream)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), testSignature) {
			t.Error("chat lost signature")
		}
		io.WriteString(w, textStream)
	}))
	defer server.Close()
	c := New(broker)
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	h := testAPI(t, c)
	first := `{"model":"google-antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"weather?"}]}`
	next := `{"model":"google-antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"weather?"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Seoul\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`
	for _, body := range []string{first, next} {
		w := apiCall(h, "/v1/chat/completions", body, "chat", "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if attempts != 2 {
		t.Fatal(attempts)
	}
}

func TestErrorsDoNotExposeUpstreamSecrets(t *testing.T) {
	_, broker := testRegistry(t)
	secret := "secret-token project-secret user@example.test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, "invalid schema "+secret)
	}))
	defer server.Close()
	c := New(broker)
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	var logs []observability.Event
	c.Logger = func(e observability.Event) { logs = append(logs, e) }
	w := apiCall(testAPI(t, c), "/v1/responses", firstPayload, "errors", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_tool_schema") {
		t.Fatal(w.Code, w.Body.String())
	}
	encoded, _ := json.Marshal(logs)
	for _, word := range strings.Fields(secret) {
		if strings.Contains(w.Body.String(), word) || strings.Contains(string(encoded), word) {
			t.Fatal("secret exposed")
		}
	}
}

func TestSameUserTextDoesNotIdentifyConversation(t *testing.T) {
	payload := map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": "same"}}}
	r := httptest.NewRequest("POST", "/", nil)
	if sessionID(r, payload) == sessionID(r, payload) {
		t.Fatal("anonymous conversations collide")
	}
	r.Header.Set("X-Client-Thread-Id", "stable")
	if sessionID(r, payload) != sessionID(r, payload) {
		t.Fatal("explicit conversation unstable")
	}
}

func TestPinnedAndToolHistoryRequestsNeverRotateOn429(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "tool-history", true: "explicit"}[explicit], func(t *testing.T) {
			_, broker := testRegistry(t)
			calls, probes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "fetchAvailableModels") {
					probes++
					w.WriteHeader(500)
					return
				}
				calls++
				if !explicit && calls == 1 {
					io.WriteString(w, toolStream)
					return
				}
				w.WriteHeader(429)
			}))
			defer server.Close()
			c := New(broker)
			c.BaseURL = server.URL
			c.HTTP = server.Client()
			h := testAPI(t, c)
			payload, selector := firstPayload, "A"
			if !explicit {
				if w := apiCall(h, "/v1/responses", firstPayload, "s", ""); w.Code != 200 {
					t.Fatal(w.Code)
				}
				payload, selector = nextPayload, ""
			}
			w := apiCall(h, "/v1/responses", payload, "s", selector)
			if w.Code != 429 || probes != 0 {
				t.Fatal(w.Code, probes)
			}
			want := 1
			if !explicit {
				want = 2
			}
			if calls != want {
				t.Fatal(calls)
			}
		})
	}
}

func TestInvalidSchemaAndSelectorRejectedBeforeUpstream(t *testing.T) {
	_, broker := testRegistry(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	c := New(broker)
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	h := testAPI(t, c)
	payload := `{"model":"google-antigravity/gemini-3.8-flash","input":"x","tools":[{"type":"function","name":"bad","parameters":{"$ref":"#/$defs/missing"}}]}`
	w := apiCall(h, "/v1/responses", payload, "schema", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_tool_schema") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = apiCall(h, "/v1/responses", firstPayload, "selector", "../A")
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if calls != 0 {
		t.Fatal("invalid input reached upstream", calls)
	}
}

func TestParallelToolCallsPreserveStepAndSingleSignature(t *testing.T) {
	_, broker := testRegistry(t)
	multiToolStream := "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[" +
		"{\"thought\":true,\"thoughtSignature\":\"" + testSignature + "\"}," +
		"{\"functionCall\":{\"id\":\"wire_1\",\"name\":\"read_file\",\"args\":{\"path\":\"a.txt\"}}}," +
		"{\"functionCall\":{\"id\":\"wire_2\",\"name\":\"read_file\",\"args\":{\"path\":\"b.txt\"}}}" +
		"]}}]}}\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "functionResponse") {
			var request map[string]any
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			envelopeRequest := request["request"].(map[string]any)
			contents := envelopeRequest["contents"].([]any)
			if strings.Count(string(body), testSignature) != 1 {
				t.Errorf("parallel step must replay thoughtSignature exactly once, body: %s", string(body))
			}
			var modelParts, responseParts int
			for _, rawContent := range contents {
				content := rawContent.(map[string]any)
				parts := content["parts"].([]any)
				if content["role"] == "model" {
					calls := 0
					for _, rawPart := range parts {
						if _, ok := rawPart.(map[string]any)["functionCall"]; ok {
							calls++
						}
					}
					if calls == 2 {
						modelParts = calls
					}
				}
				if content["role"] == "user" {
					responses := 0
					for _, rawPart := range parts {
						if _, ok := rawPart.(map[string]any)["functionResponse"]; ok {
							responses++
						}
					}
					if responses == 2 {
						responseParts = responses
					}
				}
			}
			if modelParts != 2 || responseParts != 2 {
				t.Errorf("parallel calls/results were not grouped by Gemini step: %s", string(body))
			}
			io.WriteString(w, textStream)
		} else {
			io.WriteString(w, multiToolStream)
		}
	}))
	defer server.Close()

	c := New(broker)
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	h := testAPI(t, c)

	first := apiCall(h, "/v1/responses", firstPayload, "", "")
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}

	var firstResp map[string]any
	json.Unmarshal(first.Body.Bytes(), &firstResp)
	output := firstResp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 tool calls in output, got %d", len(output))
	}
	call1 := output[0].(map[string]any)
	call2 := output[1].(map[string]any)
	id1 := call1["call_id"].(string)
	id2 := call2["call_id"].(string)

	nextTurn := map[string]any{
		"model": "google-antigravity/gemini-3.8-flash",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "read files"},
			map[string]any{"type": "function_call", "call_id": id1, "name": "read_file", "arguments": `{"path":"a.txt"}`},
			map[string]any{"type": "function_call_output", "call_id": id1, "output": "content a"},
			map[string]any{"type": "function_call", "call_id": id2, "name": "read_file", "arguments": `{"path":"b.txt"}`},
			map[string]any{"type": "function_call_output", "call_id": id2, "output": "content b"},
		},
	}
	nextTurnBytes, _ := json.Marshal(nextTurn)
	second := apiCall(h, "/v1/responses", string(nextTurnBytes), "", "")
	if second.Code != 200 {
		t.Fatalf("status=%d body=%s", second.Code, second.Body.String())
	}
}

func TestCompletedOldToolHistoryDoesNotInvalidateFreshTurn(t *testing.T) {
	replay := NewReplayCache(16, time.Hour)
	payload := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "old question"},
			map[string]any{"type": "function_call", "call_id": "call_ag_expired", "name": "weather", "arguments": "{\"city\":\"Seoul\"}"},
			map[string]any{"type": "function_call_output", "call_id": "call_ag_expired", "output": "sunny"},
			map[string]any{"type": "message", "role": "user", "content": "new question"},
		},
	}
	prepared, err := prepareRequest(payload, "gemini-3.8-flash", "-session", "google-1", replay)
	if err != nil {
		t.Fatalf("completed old history should be tolerated: %v", err)
	}
	contents := prepared.Body["contents"].([]any)
	if len(contents) != 4 {
		t.Fatalf("contents=%v", contents)
	}
}

func TestCurrentTurnStillRequiresRouterContinuity(t *testing.T) {
	replay := NewReplayCache(16, time.Hour)
	payload := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "question"},
			map[string]any{"type": "function_call", "call_id": "call_ag_missing", "name": "weather", "arguments": "{\"city\":\"Seoul\"}"},
			map[string]any{"type": "function_call_output", "call_id": "call_ag_missing", "output": "sunny"},
		},
	}
	if _, err := prepareRequest(payload, "gemini-3.8-flash", "-session", "google-1", replay); err == nil || !strings.Contains(err.Error(), "session_continuity_unavailable") {
		t.Fatalf("current turn accepted without continuity: %v", err)
	}
}

func TestCurrentGeminiStepRequiresLeadingSignature(t *testing.T) {
	replay := NewReplayCache(16, time.Hour)
	args := map[string]any{"city": "Seoul"}
	replay.RememberCall("call_ag_unsigned", "wire_1", "google-1", "gemini-3.8-flash-medium", "-session", "weather", args, "", "step-1", 0)
	payload := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "question"},
			map[string]any{"type": "function_call", "call_id": "call_ag_unsigned", "name": "weather", "arguments": "{\"city\":\"Seoul\"}"},
			map[string]any{"type": "function_call_output", "call_id": "call_ag_unsigned", "output": "sunny"},
		},
	}
	if _, err := prepareRequest(payload, "gemini-3.8-flash", "-session", "google-1", replay); err == nil || !strings.Contains(err.Error(), "leading thought signature") {
		t.Fatalf("unsigned leading call accepted: %v", err)
	}
}

func TestPersistentContinuitySurvivesRestartAndRestoresProfile(t *testing.T) {
	store, broker := testRegistry(t)
	var mu sync.Mutex
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		call := len(tokens)
		mu.Unlock()
		if call == 1 {
			io.WriteString(w, toolStream)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), testSignature) || !strings.Contains(string(body), "\"id\":\"call_1\"") {
			t.Errorf("persisted wire continuity was not restored: %s", string(body))
		}
		io.WriteString(w, textStream)
	}))
	defer server.Close()

	firstClient, err := NewPersistent(broker, store.Root())
	if err != nil {
		t.Fatal(err)
	}
	firstClient.BaseURL = server.URL
	firstClient.HTTP = server.Client()
	first := apiCall(testAPI(t, firstClient), "/v1/responses", firstPayload, "", "")
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	callID := returnedCallID(t, first)

	registry, _ := store.Load()
	registry.Use(profile.ProviderGoogleAntigravity, "B")
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPersistent(broker, store.Root())
	if err != nil {
		t.Fatal(err)
	}
	restarted.BaseURL = server.URL
	restarted.HTTP = server.Client()
	second := apiCall(testAPI(t, restarted), "/v1/responses", strings.ReplaceAll(nextPayload, "call_1", callID), "", "")
	if second.Code != 200 {
		t.Fatal(second.Code, second.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(tokens, ",") != "Bearer token-A,Bearer token-A" {
		t.Fatalf("restart changed pinned account: %v", tokens)
	}
}

func TestSlidingTTLIsPersistedAfterActivity(t *testing.T) {
	root := t.TempDir()
	path := root + "/continuity.json"
	base := time.Now()
	cache, err := NewPersistentReplayCache(16, 10*time.Minute, time.Hour, path)
	if err != nil {
		t.Fatal(err)
	}
	cache.now = func() time.Time { return base }
	args := map[string]any{"city": "Seoul"}
	cache.RememberCall("call_ag_test", "wire_1", "A", "gemini", "session", "weather", args, testSignature, "step-1", 0)
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "Seoul") {
		t.Fatal("raw tool arguments leaked into continuity state")
	}

	cache.now = func() time.Time { return base.Add(6 * time.Minute) }
	if _, ok := cache.LookupCall("call_ag_test", "gemini", "weather", args); !ok {
		t.Fatal("active call expired before idle TTL")
	}
	if !cache.BindSession("A", "gemini", "session") {
		t.Fatal("could not persist sliding activity")
	}

	restarted, err := NewPersistentReplayCache(16, 10*time.Minute, time.Hour, path)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return base.Add(11 * time.Minute) }
	if _, ok := restarted.LookupCall("call_ag_test", "gemini", "weather", args); !ok {
		t.Fatal("sliding TTL was not persisted across restart")
	}
}
