package contextobs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponseObserverExtractsOnlyNumericAndBoundedMetadata(t *testing.T) {
	key := []byte("response-observer-key")
	o := NewResponseObserver(key, 0)
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"type":"function_call","name":"private_tool","arguments":"PRIVATE_FUNCTION_ARGUMENTS"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp-private-123","output":[{"content":"PRIVATE_RESPONSE_TEXT"}],"usage":{"input_tokens":72000,"output_tokens":1200,"total_tokens":73200,"input_tokens_details":{"cached_tokens":61000},"output_tokens_details":{"reasoning_tokens":700}}}}`,
		``,
	}, "\n")

	// Exercise incremental parsing across arbitrary read boundaries.
	for i := 0; i < len(stream); i += 17 {
		end := i + 17
		if end > len(stream) {
			end = len(stream)
		}
		o.Observe([]byte(stream[i:end]))
	}
	m := o.Metrics()
	if m.SSEEventCount != 2 || m.OutputItemCount != 1 || m.ToolCallCount != 1 {
		t.Fatalf("unexpected event counts: %+v", m)
	}
	if !m.UsageAvailable || m.InputTokens != 72000 || m.CachedInputTokens != 61000 || m.OutputTokens != 1200 || m.ReasoningTokens != 700 || m.TotalTokens != 73200 {
		t.Fatalf("unexpected usage: %+v", m)
	}
	if m.ResponseRef == "" || strings.Contains(m.ResponseRef, "resp-private-123") {
		t.Fatalf("response ref must be opaque: %q", m.ResponseRef)
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PRIVATE_FUNCTION_ARGUMENTS", "PRIVATE_RESPONSE_TEXT", "resp-private-123", "private_tool"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("serialized response metrics leaked %q: %s", secret, encoded)
		}
	}
}

func TestResponseObserverSupportsTopLevelUsageVariant(t *testing.T) {
	o := NewResponseObserver([]byte("key"), 0)
	o.Observe([]byte("data: {\"type\":\"response.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":8,\"output_tokens\":2,\"reasoning_tokens\":1,\"total_tokens\":12}}\n\n"))
	m := o.Metrics()
	if !m.UsageAvailable || m.InputTokens != 10 || m.CachedInputTokens != 8 || m.OutputTokens != 2 || m.ReasoningTokens != 1 || m.TotalTokens != 12 {
		t.Fatalf("unexpected top-level usage: %+v", m)
	}
}

func TestResponseObserverDropsOversizedLineAndRecovers(t *testing.T) {
	o := NewResponseObserver([]byte("key"), 64)
	o.Observe([]byte("data: " + strings.Repeat("X", 100) + "\n"))
	o.Observe([]byte("data: {\"type\":\"response.completed\",\"usage\":{\"input_tokens\":3}}\n\n"))
	m := o.Metrics()
	if m.SSEEventCount != 1 || !m.UsageAvailable || m.InputTokens != 3 {
		t.Fatalf("observer did not recover after oversized line: %+v", m)
	}
}
