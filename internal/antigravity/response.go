package antigravity

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func translateCCAStream(w http.ResponseWriter, body io.Reader, externalModel, wireModel, profileID, session string, replay *ReplayCache) error {
	responseID := "resp_ag_" + randomHex(12)
	createdAt := time.Now().Unix()
	writeSSE(w, map[string]any{"type": "response.created", "response": map[string]any{
		"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress", "model": externalModel,
	}})

	var output []any
	var text strings.Builder
	var messageID string
	messageIndex := -1
	messageStarted := false
	nextOutputIndex := 0
	usage := map[string]any{}
	sawData := false
	sawTerminal := false
	pendingSignature := ""
	seenCalls := map[string]bool{}

	fail := func(message string) error {
		writeSSE(w, map[string]any{"type": "response.failed", "response": map[string]any{
			"id": responseID, "object": "response", "created_at": createdAt, "status": "failed", "model": externalModel,
			"error": map[string]any{"type": "upstream_error", "message": message},
		}})
		return errors.New(message)
	}

	err := scanSSE(body, func(data string) error {
		if data == "[DONE]" {
			sawTerminal = true
			return nil
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fail("Google Antigravity returned a malformed SSE event.")
		}
		sawData = true
		if _, ok := chunk["error"]; ok {
			return fail("Google Antigravity returned an upstream error.")
		}
		root := chunk
		if wrapped, ok := chunk["response"].(map[string]any); ok {
			root = wrapped
		} else if _, present := chunk["response"]; present {
			return fail("Google Antigravity returned an invalid response wrapper.")
		}
		if metadata, ok := root["usageMetadata"].(map[string]any); ok {
			usage = responsesUsage(metadata)
			sawTerminal = true
		}
		candidatesRaw, present := root["candidates"]
		if !present || candidatesRaw == nil {
			return nil
		}
		candidates, ok := candidatesRaw.([]any)
		if !ok {
			return fail("Google Antigravity returned invalid candidates.")
		}
		for _, rawCandidate := range candidates {
			candidate, ok := rawCandidate.(map[string]any)
			if !ok {
				return fail("Google Antigravity returned an invalid candidate.")
			}
			if finishReason, _ := candidate["finishReason"].(string); finishReason != "" {
				sawTerminal = true
			}
			content, _ := candidate["content"].(map[string]any)
			if content == nil {
				continue
			}
			partsRaw := content["parts"]
			if partsRaw == nil {
				continue
			}
			parts, ok := partsRaw.([]any)
			if !ok {
				return fail("Google Antigravity returned invalid content parts.")
			}
			for _, rawPart := range parts {
				part, ok := rawPart.(map[string]any)
				if !ok {
					return fail("Google Antigravity returned an invalid content part.")
				}
				signature := thoughtSignature(part)
				if thought, _ := part["thought"].(bool); thought {
					if isLikelyRealThoughtSignature(signature) {
						pendingSignature = signature
					}
					continue
				}
				if delta, ok := part["text"].(string); ok && delta != "" {
					if !messageStarted {
						messageStarted = true
						messageID = "msg_ag_" + randomHex(10)
						messageIndex = nextOutputIndex
						nextOutputIndex++
						writeSSE(w, map[string]any{"type": "response.output_item.added", "output_index": messageIndex, "item": map[string]any{
							"id": messageID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{},
						}})
						writeSSE(w, map[string]any{"type": "response.content_part.added", "item_id": messageID, "output_index": messageIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
					}
					text.WriteString(delta)
					writeSSE(w, map[string]any{"type": "response.output_text.delta", "item_id": messageID, "output_index": messageIndex, "content_index": 0, "delta": delta})
				}
				if call, ok := part["functionCall"].(map[string]any); ok {
					name, _ := call["name"].(string)
					if name == "" {
						return fail("Google Antigravity returned a function call without a name.")
					}
					args := call["args"]
					encodedArgs, err := json.Marshal(args)
					if err != nil {
						return fail("Google Antigravity returned invalid function arguments.")
					}
					callID, _ := call["id"].(string)
					if callID == "" {
						callID = "call_ag_" + randomHex(10)
					}
					identity := name + "\x00" + string(encodedArgs) + "\x00" + callID
					if seenCalls[identity] {
						continue
					}
					seenCalls[identity] = true
					itemID := "fc_ag_" + randomHex(10)
					index := nextOutputIndex
					nextOutputIndex++
					if !isLikelyRealThoughtSignature(signature) {
						signature = pendingSignature
					}
					if isLikelyRealThoughtSignature(signature) {
						replay.Remember(profileID, wireModel, session, name, args, signature)
					}
					writeSSE(w, map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{
						"id": itemID, "type": "function_call", "status": "in_progress", "call_id": callID, "name": name, "arguments": "",
					}})
					writeSSE(w, map[string]any{"type": "response.function_call_arguments.delta", "item_id": itemID, "output_index": index, "delta": string(encodedArgs)})
					item := map[string]any{"id": itemID, "type": "function_call", "status": "completed", "call_id": callID, "name": name, "arguments": string(encodedArgs)}
					writeSSE(w, map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
					output = append(output, item)
					pendingSignature = ""
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !sawData || !sawTerminal {
		return fail("Google Antigravity stream ended before a terminal response.")
	}
	if messageStarted {
		part := map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}}
		writeSSE(w, map[string]any{"type": "response.content_part.done", "item_id": messageID, "output_index": messageIndex, "content_index": 0, "part": part})
		item := map[string]any{"id": messageID, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}
		writeSSE(w, map[string]any{"type": "response.output_item.done", "output_index": messageIndex, "item": item})
		output = append(output, item)
	}
	response := map[string]any{
		"id": responseID, "object": "response", "created_at": createdAt, "status": "completed", "model": externalModel, "output": output,
	}
	if len(usage) > 0 {
		response["usage"] = usage
	}
	writeSSE(w, map[string]any{"type": "response.completed", "response": response})
	return nil
}

func scanSSE(r io.Reader, fn func(string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		value := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		return fn(value)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Antigravity SSE: %w", err)
	}
	return flush()
}

func responsesUsage(metadata map[string]any) map[string]any {
	input := numericInt64(metadata["promptTokenCount"])
	output := numericInt64(metadata["candidatesTokenCount"])
	reasoning := numericInt64(metadata["thoughtsTokenCount"])
	total := numericInt64(metadata["totalTokenCount"])
	if total == 0 {
		total = input + output + reasoning
	}
	usage := map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": total}
	if cached := numericInt64(metadata["cachedContentTokenCount"]); cached > 0 {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoning > 0 {
		usage["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return usage
}

func numericInt64(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

func thoughtSignature(part map[string]any) string {
	for _, key := range []string{"thoughtSignature", "thought_signature"} {
		if value, _ := part[key].(string); value != "" {
			return value
		}
	}
	return ""
}

func writeSSE(w http.ResponseWriter, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
