package antigravity

import (
	"encoding/json"
	"fmt"
	"strings"
)

type preparedRequest struct {
	WireModel string
	Body      map[string]any
}

func prepareRequest(payload map[string]any, upstreamModel, session, profileID string, replay *ReplayCache) (preparedRequest, error) {
	wireModel, thinkingLevel := resolveWireModel(upstreamModel, reasoningEffort(payload))
	contents, systemText, err := responsesInputToGemini(payload["input"], profileID, wireModel, session, replay)
	if err != nil {
		return preparedRequest{}, err
	}
	if instructions, _ := payload["instructions"].(string); strings.TrimSpace(instructions) != "" {
		systemText = joinInstruction(systemText, instructions)
	}
	body := map[string]any{"contents": contents, "sessionId": session}
	if systemText != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": systemText}}}
	}
	if tools, err := translateTools(payload["tools"]); err != nil {
		return preparedRequest{}, err
	} else if tools != nil {
		body["tools"] = tools
	}
	if config := translateGenerationConfig(payload, thinkingLevel); len(config) > 0 {
		body["generationConfig"] = config
	}
	if toolConfig, err := translateToolChoice(payload["tool_choice"], wireModel); err != nil {
		return preparedRequest{}, err
	} else if toolConfig != nil {
		body["toolConfig"] = toolConfig
	} else if strings.Contains(strings.ToLower(wireModel), "claude") && body["tools"] != nil {
		body["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "VALIDATED"}}
	}
	return preparedRequest{WireModel: wireModel, Body: body}, nil
}

func responsesInputToGemini(raw any, profileID, model, session string, replay *ReplayCache) ([]any, string, error) {
	input, ok := raw.([]any)
	if !ok {
		return nil, "", fmt.Errorf("input must be a Responses input array")
	}
	contents := make([]any, 0, len(input))
	systemText := ""
	callNames := map[string]string{}
	callWireIDs := map[string]string{}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("input items must be JSON objects")
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "message", "":
			role, _ := item["role"].(string)
			parts, text, err := messageParts(item["content"])
			if err != nil {
				return nil, "", err
			}
			switch role {
			case "system", "developer":
				systemText = joinInstruction(systemText, text)
			case "assistant":
				if len(parts) > 0 {
					contents = append(contents, map[string]any{"role": "model", "parts": parts})
				}
			case "user":
				if len(parts) > 0 {
					contents = append(contents, map[string]any{"role": "user", "parts": parts})
				}
			default:
				return nil, "", fmt.Errorf("unsupported message role %q", role)
			}
		case "function_call":
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			if name == "" || callID == "" {
				return nil, "", fmt.Errorf("function_call requires call_id and name")
			}
			var args any = map[string]any{}
			if strings.TrimSpace(arguments) != "" {
				if err := json.Unmarshal([]byte(arguments), &args); err != nil {
					return nil, "", fmt.Errorf("function_call %s has invalid JSON arguments", name)
				}
			}
			callNames[callID] = name
			wireID := callID
			signature, found := replay.Lookup(profileID, model, session, name, args)
			if strings.HasPrefix(callID, "call_ag_") {
				record, ok := replay.LookupCall(callID, model, name, args)
				if !ok || record.profile != profileID || record.session != session {
					return nil, "", fmt.Errorf("session_continuity_unavailable: invalid or expired tool call")
				}
				wireID, signature = record.wireID, record.signature
				found = signature != ""
			}
			callWireIDs[callID] = wireID
			part := map[string]any{"functionCall": map[string]any{"id": wireID, "name": name, "args": args}}
			if found {
				part["thoughtSignature"] = signature
			} else if strings.Contains(strings.ToLower(model), "gemini") {
				return nil, "", fmt.Errorf("session_continuity_unavailable: tool signature expired or conversation identity changed")
			}
			contents = append(contents, map[string]any{"role": "model", "parts": []any{part}})
		case "function_call_output":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				return nil, "", fmt.Errorf("function_call_output requires call_id")
			}
			name := callNames[callID]
			if name == "" {
				return nil, "", fmt.Errorf("function_call_output requires matching function_call history")
			}
			response := map[string]any{"result": item["output"]}
			part := map[string]any{"functionResponse": map[string]any{"id": callWireIDs[callID], "name": name, "response": response}}
			contents = append(contents, map[string]any{"role": "user", "parts": []any{part}})
		default:
			return nil, "", fmt.Errorf("unsupported Responses input item type %q", typ)
		}
	}
	return contents, systemText, nil
}

func messageParts(raw any) ([]any, string, error) {
	if raw == nil {
		return nil, "", nil
	}
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"text": text}}, text, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, "", fmt.Errorf("message content must be text or an array")
	}
	parts := make([]any, 0, len(items))
	var text strings.Builder
	for _, rawPart := range items {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("message content parts must be objects")
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "input_text", "output_text", "text", "":
			value, _ := part["text"].(string)
			parts = append(parts, map[string]any{"text": value})
			text.WriteString(value)
		case "input_image":
			return nil, "", fmt.Errorf("google-antigravity image input is not implemented yet")
		default:
			return nil, "", fmt.Errorf("unsupported message content type %q", typ)
		}
	}
	return parts, text.String(), nil
}

func translateTools(raw any) (any, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("tools must be an array")
	}
	declarations := make([]any, 0, len(items))
	for _, rawTool := range items {
		tool, ok := rawTool.(map[string]any)
		if !ok || tool["type"] != "function" {
			return nil, fmt.Errorf("only function tools are supported by google-antigravity")
		}
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("function tool name is required")
		}
		declaration := map[string]any{"name": name}
		if description, _ := tool["description"].(string); description != "" {
			declaration["description"] = description
		}
		if parameters := tool["parameters"]; parameters != nil {
			converted, err := transformGoogleSchema(parameters)
			if err != nil {
				return nil, fmt.Errorf("invalid_tool_schema: %w", err)
			}
			declaration["parameters"] = converted
		}
		declarations = append(declarations, declaration)
	}
	if len(declarations) == 0 {
		return nil, nil
	}
	return []any{map[string]any{"functionDeclarations": declarations}}, nil
}

func translateToolChoice(raw any, wireModel string) (any, error) {
	if raw == nil {
		return nil, nil
	}
	config := map[string]any{}
	switch choice := raw.(type) {
	case string:
		switch choice {
		case "auto":
			config["mode"] = "AUTO"
		case "none":
			config["mode"] = "NONE"
		case "required":
			config["mode"] = "ANY"
		default:
			return nil, fmt.Errorf("unsupported tool_choice %q", choice)
		}
	case map[string]any:
		name, _ := choice["name"].(string)
		if name == "" {
			if function, _ := choice["function"].(map[string]any); function != nil {
				name, _ = function["name"].(string)
			}
		}
		if name == "" {
			return nil, fmt.Errorf("function tool_choice requires a name")
		}
		config["mode"] = "ANY"
		config["allowedFunctionNames"] = []any{name}
	default:
		return nil, fmt.Errorf("invalid tool_choice")
	}
	if strings.Contains(strings.ToLower(wireModel), "claude") && config["mode"] != "NONE" {
		config["mode"] = "VALIDATED"
	}
	return map[string]any{"functionCallingConfig": config}, nil
}

func translateGenerationConfig(payload map[string]any, thinkingLevel string) map[string]any {
	config := map[string]any{}
	if value, ok := payload["temperature"].(float64); ok {
		config["temperature"] = value
	}
	if value, ok := payload["top_p"].(float64); ok {
		config["topP"] = value
	}
	if value, ok := payload["max_output_tokens"].(float64); ok && value > 0 {
		config["maxOutputTokens"] = int64(value)
	}
	if thinkingLevel != "" {
		config["thinkingConfig"] = map[string]any{"thinkingLevel": thinkingLevel, "includeThoughts": true}
	}
	return config
}

func reasoningEffort(payload map[string]any) string {
	if reasoning, _ := payload["reasoning"].(map[string]any); reasoning != nil {
		if effort, _ := reasoning["effort"].(string); effort != "" {
			return strings.ToLower(effort)
		}
	}
	return ""
}

func resolveWireModel(model, effort string) (string, string) {
	switch model {
	case "gemini-3.8-flash":
		if effort != "low" && effort != "high" {
			effort = "medium"
		}
		return model + "-" + effort, ""
	case "gemini-3.7-flash":
		if effort != "low" && effort != "high" {
			effort = "medium"
		}
		return model + "-tiered", effort
	case "gemini-3.1-pro":
		if effort == "low" {
			return "gemini-3.1-pro-low", ""
		}
		return "gemini-pro-agent", ""
	default:
		if strings.Contains(strings.ToLower(model), "claude") && effort != "" {
			return model, effort
		}
		return model, ""
	}
}

func joinInstruction(current, next string) string {
	current = strings.TrimSpace(current)
	next = strings.TrimSpace(next)
	if current == "" {
		return next
	}
	if next == "" {
		return current
	}
	return current + "\n\n" + next
}
