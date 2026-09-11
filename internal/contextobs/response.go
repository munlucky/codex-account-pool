package contextobs

import (
	"encoding/json"
	"strings"
)

const defaultMaxSSELineBytes = 1 << 20 // 1 MiB

// ResponseObserver incrementally consumes SSE bytes and keeps only numeric/bounded metadata.
type ResponseObserver struct {
	key     []byte
	line    []byte
	discard bool
	maxLine int
	metrics ResponseMetrics
}

func NewResponseObserver(key []byte, maxLineBytes int) *ResponseObserver {
	if maxLineBytes <= 0 {
		maxLineBytes = defaultMaxSSELineBytes
	}
	return &ResponseObserver{
		key:     append([]byte(nil), key...),
		line:    make([]byte, 0, 256),
		maxLine: maxLineBytes,
	}
}

func (o *ResponseObserver) Observe(p []byte) {
	for _, b := range p {
		if b == '\n' {
			if !o.discard {
				o.observeLine(o.line)
			}
			o.line = o.line[:0]
			o.discard = false
			continue
		}
		if o.discard {
			continue
		}
		if len(o.line) >= o.maxLine {
			o.line = o.line[:0]
			o.discard = true
			continue
		}
		o.line = append(o.line, b)
	}
}

func (o *ResponseObserver) Metrics() ResponseMetrics {
	return o.metrics
}

func (o *ResponseObserver) observeLine(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}
	o.metrics.SSEEventCount++

	var eventType string
	_ = json.Unmarshal(event["type"], &eventType)
	if eventType == "response.output_item.done" {
		o.metrics.OutputItemCount++
		if isToolCallItem(event["item"]) {
			o.metrics.ToolCallCount++
		}
	}

	if raw := event["response"]; len(raw) > 0 {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil {
			o.captureResponseID(response["id"])
			o.captureUsage(response["usage"])
		}
	}
	// Some backend variants expose usage at the event top level.
	o.captureUsage(event["usage"])
}

func (o *ResponseObserver) captureResponseID(raw json.RawMessage) {
	if len(raw) == 0 || len(o.key) == 0 {
		return
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || strings.TrimSpace(id) == "" {
		return
	}
	a, err := NewAnalyzer(o.key, 1)
	if err != nil {
		return
	}
	o.metrics.ResponseRef = a.fingerprint("r_", []byte(id))
}

func (o *ResponseObserver) captureUsage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil || usage == nil {
		return
	}
	seen := false
	if v, ok := readInt64(usage["input_tokens"]); ok {
		o.metrics.InputTokens = v
		seen = true
	}
	if v, ok := readInt64(usage["cached_input_tokens"]); ok {
		o.metrics.CachedInputTokens = v
		seen = true
	}
	if v, ok := readInt64(usage["output_tokens"]); ok {
		o.metrics.OutputTokens = v
		seen = true
	}
	if v, ok := readInt64(usage["reasoning_tokens"]); ok {
		o.metrics.ReasoningTokens = v
		seen = true
	}
	if v, ok := readInt64(usage["total_tokens"]); ok {
		o.metrics.TotalTokens = v
		seen = true
	}
	if rawDetails := usage["input_tokens_details"]; len(rawDetails) > 0 {
		var details map[string]json.RawMessage
		if json.Unmarshal(rawDetails, &details) == nil {
			if v, ok := readInt64(details["cached_tokens"]); ok {
				o.metrics.CachedInputTokens = v
				seen = true
			}
		}
	}
	if rawDetails := usage["output_tokens_details"]; len(rawDetails) > 0 {
		var details map[string]json.RawMessage
		if json.Unmarshal(rawDetails, &details) == nil {
			if v, ok := readInt64(details["reasoning_tokens"]); ok {
				o.metrics.ReasoningTokens = v
				seen = true
			}
		}
	}
	if seen {
		o.metrics.UsageAvailable = true
	}
}

func readInt64(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

func isToolCallItem(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var item struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return false
	}
	t := strings.ToLower(strings.TrimSpace(item.Type))
	return t == "function_call" || t == "tool_call" || strings.HasSuffix(t, "_call")
}
