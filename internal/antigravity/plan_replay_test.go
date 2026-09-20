package antigravity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const redactedPlan = `[Plan approved and saved to C:\Users\test\.qwen\plans\session.md. The plan text was removed from the conversation after approval; read that file if you need to consult it again.]`

func TestApprovedPlanReplayAcrossRestart(t *testing.T) {
	_, broker := testRegistry(t)
	root := t.TempDir()
	args := map[string]any{"plan": "Original approved plan", "originalRequest": "keep this"}
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "fetchAvailableModels") {
			io.WriteString(w, `{"models":{"gemini-3.8-flash":{"quotaInfo":{"remainingFraction":1}}}}`)
			return
		}
		count++
		w.Header().Set("Content-Type", "text/event-stream")
		if count == 1 {
			data, _ := json.Marshal(map[string]any{"response": map[string]any{"candidates": []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"parts": []any{map[string]any{"functionCall": map[string]any{"id": "wire_plan", "name": "exit_plan_mode", "args": args}, "thoughtSignature": testSignature}}}}}}})
			io.WriteString(w, "data: "+string(data)+"\n\n")
			return
		}
		var envelope map[string]any
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Error(err)
			return
		}
		request := envelope["request"].(map[string]any)
		found := false
		for _, raw := range request["contents"].([]any) {
			for _, part := range raw.(map[string]any)["parts"].([]any) {
				p := part.(map[string]any)
				if fc, ok := p["functionCall"].(map[string]any); ok {
					found = true
					if canonicalArgs(fc["args"]) != canonicalArgs(args) || fc["id"] != "wire_plan" || p["thoughtSignature"] != testSignature {
						t.Error("upstream replay did not preserve original arguments, ID and signature")
					}
				}
			}
		}
		if !found {
			t.Error("missing replayed function call")
		}
		io.WriteString(w, textStream)
	}))
	defer server.Close()
	makeClient := func() *Client {
		c, err := NewPersistent(broker, root)
		if err != nil {
			t.Fatal(err)
		}
		c.BaseURL, c.HTTP = server.URL, server.Client()
		return c
	}
	c := makeClient()
	first := apiCall(testAPI(t, c), "/v1/responses", firstPayload, "", "")
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	id := returnedCallID(t, first)
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "weather?"},
		map[string]any{"type": "function_call", "call_id": id, "name": "exit_plan_mode", "arguments": canonicalArgs(map[string]any{"plan": redactedPlan, "originalRequest": "keep this"})},
		map[string]any{"type": "function_call_output", "call_id": id, "output": "User approved. You can now start coding."},
	}
	for _, restart := range []bool{false, true} {
		if restart {
			c = makeClient()
		}
		payload, _ := json.Marshal(map[string]any{"model": "google-antigravity/gemini-3.8-flash", "input": input})
		response := apiCall(testAPI(t, c), "/v1/responses", string(payload), "", "")
		if response.Code != 200 {
			t.Fatal(restart, response.Code, response.Body.String())
		}
	}
	if count != 3 {
		t.Fatalf("upstream requests = %d", count)
	}
}

func TestPlanReplayRejectsUnrelatedChanges(t *testing.T) {
	c := NewReplayCache(0, 0)
	original := map[string]any{"plan": "original", "originalRequest": "keep"}
	c.RememberCall("call_ag_plan", "wire", "A", "gemini", "session", "exit_plan_mode", original, testSignature, "step", 0)
	for _, args := range []any{
		map[string]any{"plan": "arbitrary edit", "originalRequest": "keep"},
		map[string]any{"plan": redactedPlan, "originalRequest": "changed"},
		map[string]any{"plan": redactedPlan},
		map[string]any{"plan": redactedPlan, "originalRequest": "keep", "extra": true},
	} {
		if _, ok := c.LookupCall("call_ag_plan", "gemini", "exit_plan_mode", args); ok {
			t.Fatal("accepted changed arguments")
		}
	}
	redacted := map[string]any{"plan": redactedPlan, "originalRequest": "keep"}
	for _, name := range []string{"weather", "ExitPlanMode"} {
		if _, ok := c.LookupCall("call_ag_plan", "gemini", name, redacted); ok {
			t.Fatal("accepted wrong tool name")
		}
	}
	if _, ok := c.LookupCall("call_ag_unknown", "gemini", "exit_plan_mode", redacted); ok {
		t.Fatal("accepted unknown ID")
	}
	if _, ok := c.LookupCall("call_ag_plan", "other-model", "exit_plan_mode", redacted); ok {
		t.Fatal("accepted wrong model")
	}
	c.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if _, ok := c.LookupCall("call_ag_plan", "gemini", "exit_plan_mode", redacted); ok {
		t.Fatal("accepted expired ID")
	}
}

func TestLegacyPlanReplayRequiresOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "continuity.json")
	c, err := NewPersistentReplayCache(0, 0, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	c.RememberCall("call_ag_plan", "wire", "A", "gemini", "session", "exit_plan_mode", map[string]any{"plan": "original"}, testSignature, "step", 0)
	r := c.calls["call_ag_plan"]
	r.originalArgs = nil
	c.calls["call_ag_plan"] = r
	if err := c.persistLocked(); err != nil {
		t.Fatal(err)
	}
	c, err = NewPersistentReplayCache(0, 0, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.LookupCall("call_ag_plan", "gemini", "exit_plan_mode", map[string]any{"plan": redactedPlan}); ok {
		t.Fatal("fabricated legacy original")
	}
	if _, ok := c.LookupCall("call_ag_plan", "gemini", "exit_plan_mode", map[string]any{"plan": "original"}); !ok {
		t.Fatal("rejected exact legacy arguments")
	}
}
