package openaiapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []json.RawMessage `json:"messages"`
	Tools               []chatTool        `json:"tools,omitempty"`
	ToolChoice          json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls,omitempty"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *streamOptions    `json:"stream_options,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int              `json:"max_tokens,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	TopP                *float64          `json:"top_p,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
	ResponseFormat      json.RawMessage   `json:"response_format,omitempty"`
	User                string            `json:"user,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}
type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
		Strict      *bool           `json:"strict,omitempty"`
	} `json:"function"`
}

var supportedChatFields = map[string]bool{
	"model": true, "messages": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "stream": true, "stream_options": true,
	"max_completion_tokens": true, "max_tokens": true, "temperature": true, "top_p": true,
	"reasoning_effort": true, "response_format": true, "user": true,
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeError(w, 400, "invalid_request_error", "Could not read request body.")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, 400, "invalid_request_error", "Request body must be valid JSON.")
		return
	}
	for key := range raw {
		if !supportedChatFields[key] {
			writeError(w, 400, "invalid_request_error", fmt.Sprintf("Unsupported Chat Completions field %q.", key))
			return
		}
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "invalid_request_error", "Invalid Chat Completions request.")
		return
	}
	responsesBody, err := translateChatRequest(req)
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	clone := h.cloneCodexRequest(r, "/backend-api/codex/responses", "openai_chat_completions")
	clone.Body = io.NopCloser(bytes.NewReader(responsesBody))
	clone.ContentLength = int64(len(responsesBody))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(responsesBody)), nil }
	clone.Header.Set("Content-Type", "application/json")
	clone.Header.Del("Content-Encoding")
	clone.Header.Del("Accept-Encoding")

	if req.Stream {
		sw := newChatStreamWriter(w, req.Model, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		h.backend.ServeHTTP(sw, clone)
		sw.finish()
		return
	}
	capture := newCaptureWriter()
	h.backend.ServeHTTP(capture, clone)
	if capture.status >= 400 {
		copyCaptured(w, capture)
		return
	}
	response, err := translateNonStreamResponse(capture.body.Bytes(), capture.header.Get("Content-Type"), req.Model)
	if err != nil {
		writeError(w, 502, "api_error", "Could not translate upstream Responses payload.")
		return
	}
	writeJSON(w, 200, response)
}

func translateChatRequest(req chatRequest) ([]byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}
	input := make([]any, 0, len(req.Messages))
	for _, raw := range req.Messages {
		var msg struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id,omitempty"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		}
		if json.Unmarshal(raw, &msg) != nil || strings.TrimSpace(msg.Role) == "" {
			return nil, fmt.Errorf("each message requires a valid role")
		}
		if msg.Role == "tool" {
			if msg.ToolCallID == "" {
				return nil, fmt.Errorf("tool messages require tool_call_id")
			}
			text, err := messageContentText(msg.Content)
			if err != nil {
				return nil, err
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": msg.ToolCallID, "output": text})
			continue
		}
		if len(msg.Content) > 0 && string(msg.Content) != "null" {
			content, err := translateMessageContent(msg.Content)
			if err != nil {
				return nil, err
			}
			if content != nil {
				input = append(input, map[string]any{"type": "message", "role": msg.Role, "content": content})
			}
		}
		if msg.Role == "assistant" {
			for _, call := range msg.ToolCalls {
				if call.Type != "" && call.Type != "function" {
					return nil, fmt.Errorf("only function tool calls are supported")
				}
				if call.ID == "" || call.Function.Name == "" {
					return nil, fmt.Errorf("assistant tool calls require id and function name")
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		}
	}
	payload := map[string]any{"model": req.Model, "input": input, "store": false, "stream": true}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			if tool.Type != "function" || tool.Function.Name == "" {
				return nil, fmt.Errorf("only named function tools are supported")
			}
			mapped := map[string]any{"type": "function", "name": tool.Function.Name}
			if tool.Function.Description != "" {
				mapped["description"] = tool.Function.Description
			}
			if len(tool.Function.Parameters) > 0 {
				var p any
				if json.Unmarshal(tool.Function.Parameters, &p) != nil {
					return nil, fmt.Errorf("invalid function parameters")
				}
				mapped["parameters"] = p
			}
			if tool.Function.Strict != nil {
				mapped["strict"] = *tool.Function.Strict
			}
			tools = append(tools, mapped)
		}
		payload["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		choice, err := translateToolChoice(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		payload["tool_choice"] = choice
	}
	if req.ParallelToolCalls != nil {
		payload["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.MaxCompletionTokens != nil && req.MaxTokens != nil && *req.MaxCompletionTokens != *req.MaxTokens {
		return nil, fmt.Errorf("max_completion_tokens and max_tokens must not conflict")
	}
	if req.MaxCompletionTokens != nil {
		payload["max_output_tokens"] = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		payload["max_output_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		payload["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	if len(req.ResponseFormat) > 0 {
		var format map[string]any
		if json.Unmarshal(req.ResponseFormat, &format) != nil {
			return nil, fmt.Errorf("invalid response_format")
		}
		typ, _ := format["type"].(string)
		switch typ {
		case "text", "":
		case "json_object":
			payload["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		case "json_schema":
			js, ok := format["json_schema"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("json_schema response_format requires json_schema")
			}
			mapped := map[string]any{"type": "json_schema"}
			for _, k := range []string{"name", "schema", "strict", "description"} {
				if v, ok := js[k]; ok {
					mapped[k] = v
				}
			}
			payload["text"] = map[string]any{"format": mapped}
		default:
			return nil, fmt.Errorf("unsupported response_format type %q", typ)
		}
	}
	return json.Marshal(payload)
}

func translateMessageContent(raw json.RawMessage) (any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return nil, fmt.Errorf("message content must be text or a content array")
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		typ, _ := part["type"].(string)
		switch typ {
		case "text":
			out = append(out, map[string]any{"type": "input_text", "text": part["text"]})
		case "image_url":
			image, ok := part["image_url"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid image_url content")
			}
			out = append(out, map[string]any{"type": "input_image", "image_url": image["url"]})
		default:
			return nil, fmt.Errorf("unsupported message content type %q", typ)
		}
	}
	return out, nil
}

func messageContentText(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	return "", fmt.Errorf("tool message content must be text")
}

func translateToolChoice(raw json.RawMessage) (any, error) {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		switch value {
		case "auto", "none", "required":
			return value, nil
		default:
			return nil, fmt.Errorf("unsupported tool_choice %q", value)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil || obj.Type != "function" || obj.Function.Name == "" {
		return nil, fmt.Errorf("invalid function tool_choice")
	}
	return map[string]any{"type": "function", "name": obj.Function.Name}, nil
}

func translateNonStreamResponse(body []byte, contentType, requestedModel string) (map[string]any, error) {
	var response map[string]any
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		response = completedResponseFromSSE(body)
	} else if json.Unmarshal(body, &response) != nil {
		return nil, fmt.Errorf("invalid responses JSON")
	}
	if response == nil {
		return nil, fmt.Errorf("missing completed response")
	}
	id, _ := response["id"].(string)
	if id == "" {
		id = "chatcmpl-local"
	}
	model, _ := response["model"].(string)
	if model == "" {
		model = requestedModel
	}
	created := time.Now().Unix()
	if v, ok := numberInt64(response["created_at"]); ok {
		created = v
	}
	content := ""
	toolCalls := []any{}
	if output, ok := response["output"].([]any); ok {
		for _, rawItem := range output {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "message":
				if parts, ok := item["content"].([]any); ok {
					for _, rp := range parts {
						if p, ok := rp.(map[string]any); ok {
							if t, ok := p["text"].(string); ok {
								content += t
							}
						}
					}
				}
			case "function_call":
				callID, _ := item["call_id"].(string)
				name, _ := item["name"].(string)
				args, _ := item["arguments"].(string)
				toolCalls = append(toolCalls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": args}})
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finish = "tool_calls"
	}
	result := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}}
	if usage, ok := response["usage"].(map[string]any); ok {
		result["usage"] = chatUsage(usage)
	}
	return result, nil
}

func completedResponseFromSSE(body []byte) map[string]any {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 16<<20)
	output := make([]any, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "response.output_item.done":
			if item, ok := ev["item"].(map[string]any); ok {
				output = append(output, item)
			}
		case "response.completed":
			if response, ok := ev["response"].(map[string]any); ok {
				if existing, ok := response["output"].([]any); !ok || len(existing) == 0 {
					response["output"] = output
				}
				return response
			}
		}
	}
	return nil
}

func chatUsage(usage map[string]any) map[string]any {
	in, _ := numberInt64(usage["input_tokens"])
	out, _ := numberInt64(usage["output_tokens"])
	total, _ := numberInt64(usage["total_tokens"])
	if total == 0 {
		total = in + out
	}
	result := map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": total}
	if d, ok := usage["input_tokens_details"].(map[string]any); ok {
		if c, ok := numberInt64(d["cached_tokens"]); ok {
			result["prompt_tokens_details"] = map[string]any{"cached_tokens": c}
		}
	}
	if d, ok := usage["output_tokens_details"].(map[string]any); ok {
		if c, ok := numberInt64(d["reasoning_tokens"]); ok {
			result["completion_tokens_details"] = map[string]any{"reasoning_tokens": c}
		}
	}
	return result
}

func numberInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, e := n.Int64()
		return i, e == nil
	case int64:
		return n, true
	case int:
		return int64(n), true
	case string:
		i, e := strconv.ParseInt(n, 10, 64)
		return i, e == nil
	}
	return 0, false
}

type chatStreamWriter struct {
	dst          http.ResponseWriter
	model        string
	includeUsage bool
	header       http.Header
	status       int
	wroteHeader  bool
	buf          []byte
	started      bool
	done         bool
	toolSeen     bool
	callIndexes  map[string]int
	nextIndex    int
}

func newChatStreamWriter(dst http.ResponseWriter, model string, includeUsage bool) *chatStreamWriter {
	return &chatStreamWriter{dst: dst, model: model, includeUsage: includeUsage, header: make(http.Header), status: 200, callIndexes: map[string]int{}}
}
func (w *chatStreamWriter) Header() http.Header { return w.header }
func (w *chatStreamWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	if code >= 400 {
		copyHeader(w.dst.Header(), w.header)
		w.dst.WriteHeader(code)
		return
	}
	w.dst.Header().Set("Content-Type", "text/event-stream")
	w.dst.Header().Set("Cache-Control", "no-cache")
	w.dst.WriteHeader(code)
}
func (w *chatStreamWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.status >= 400 {
		return w.dst.Write(p)
	}
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.Index(w.buf, []byte("\n\n"))
		sep := 2
		if idx < 0 {
			idx = bytes.Index(w.buf, []byte("\r\n\r\n"))
			sep = 4
		}
		if idx < 0 {
			break
		}
		frame := append([]byte(nil), w.buf[:idx]...)
		w.buf = w.buf[idx+sep:]
		w.consumeFrame(frame)
	}
	return len(p), nil
}
func (w *chatStreamWriter) Flush() {
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *chatStreamWriter) finish() {
	if w.status < 400 && !w.done && len(bytes.TrimSpace(w.buf)) > 0 {
		w.consumeFrame(w.buf)
	}
	if w.status < 400 && !w.done {
		w.emitFinal(nil)
	}
}
func (w *chatStreamWriter) consumeFrame(frame []byte) {
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			data = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			break
		}
	}
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var ev map[string]any
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	typ, _ := ev["type"].(string)
	switch typ {
	case "response.created":
		w.emitDelta(map[string]any{"role": "assistant"}, nil)
	case "response.output_text.delta":
		if d, ok := ev["delta"].(string); ok {
			w.emitDelta(map[string]any{"content": d}, nil)
		}
	case "response.output_item.added":
		if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
			id, _ := item["id"].(string)
			if id == "" {
				id, _ = item["call_id"].(string)
			}
			idx := w.nextIndex
			w.nextIndex++
			w.callIndexes[id] = idx
			w.toolSeen = true
			name, _ := item["name"].(string)
			callID, _ := item["call_id"].(string)
			w.emitDelta(map[string]any{"tool_calls": []any{map[string]any{"index": idx, "id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": ""}}}}, nil)
		}
	case "response.function_call_arguments.delta":
		id, _ := ev["item_id"].(string)
		idx, ok := w.callIndexes[id]
		if !ok {
			if oi, ok2 := numberInt64(ev["output_index"]); ok2 {
				idx = int(oi)
			} else {
				idx = 0
			}
		}
		d, _ := ev["delta"].(string)
		w.toolSeen = true
		w.emitDelta(map[string]any{"tool_calls": []any{map[string]any{"index": idx, "function": map[string]any{"arguments": d}}}}, nil)
	case "response.completed":
		var usage map[string]any
		if r, ok := ev["response"].(map[string]any); ok {
			if u, ok := r["usage"].(map[string]any); ok {
				usage = chatUsage(u)
			}
		}
		w.emitFinal(usage)
	case "response.failed", "response.incomplete":
		w.emitFinal(nil)
	}
}
func (w *chatStreamWriter) emitDelta(delta map[string]any, finish any) {
	if w.done {
		return
	}
	w.started = true
	chunk := map[string]any{"id": "chatcmpl-stream", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": w.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	w.emit(chunk)
}
func (w *chatStreamWriter) emitFinal(usage map[string]any) {
	if w.done {
		return
	}
	finish := "stop"
	if w.toolSeen {
		finish = "tool_calls"
	}
	chunk := map[string]any{"id": "chatcmpl-stream", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": w.model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}}
	if w.includeUsage && usage != nil {
		chunk["usage"] = usage
	}
	w.emit(chunk)
	_, _ = io.WriteString(w.dst, "data: [DONE]\n\n")
	w.Flush()
	w.done = true
}
func (w *chatStreamWriter) emit(value any) {
	b, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w.dst, "data: %s\n\n", b)
	w.Flush()
}
func copyHeader(dst, src http.Header) {
	for k, values := range src {
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}
