package antigravity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func sessionID(r *http.Request, payload map[string]any) string {
	if id := explicitSessionID(r, payload); id != "" {
		return id
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		value := binary.BigEndian.Uint64(raw[:]) & 0x7fffffffffffffff
		return fmt.Sprintf("-%d", value)
	}
	return ""
}

func explicitSessionID(r *http.Request, payload map[string]any) string {
	for _, header := range []string{"x-codex-parent-thread-id", "x-client-thread-id", "x-openai-conversation-id"} {
		if r != nil {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				return stableSessionID("thread:" + value)
			}
		}
	}
	if value, _ := payload["prompt_cache_key"].(string); strings.TrimSpace(value) != "" {
		return stableSessionID("prompt-cache:" + strings.TrimSpace(value))
	}
	return ""
}

type continuityRoute struct {
	session   string
	wireModel string
}

func (c *Client) resolveContinuity(r *http.Request, payload map[string]any, requestedModel string) (continuityRoute, error) {
	explicitSession := explicitSessionID(r, payload)
	if !hasToolHistory(payload) {
		session := explicitSession
		if session == "" {
			session = sessionID(r, payload)
		}
		return continuityRoute{session: session, wireModel: requestedModel}, nil
	}

	input, _ := payload["input"].([]any)
	currentStart := currentTurnStart(input)

	// The router-issued call handle is authoritative for the provider route that
	// created an active tool step. This prevents a client-side reasoning-effort
	// change from turning the same Gemini conversation into a different wire
	// model in the middle of a signed function-calling turn.
	session := explicitSession
	wireModel := ""
	currentCalls := 0
	for i := currentStart; i < len(input); i++ {
		item, _ := input[i].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			continue
		}
		id, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		if !strings.HasPrefix(id, "call_ag_") {
			continue
		}
		record, ok := c.replay().LookupCallIdentity(id, name, args)
		if !ok ||
			(session != "" && record.session != session) ||
			(wireModel != "" && record.model != wireModel) {
			return continuityRoute{}, errors.New("session_continuity_unavailable")
		}
		if session == "" {
			session = record.session
		}
		wireModel = record.model
		currentCalls++
	}
	if currentCalls > 0 {
		if !sameContinuityModelFamily(requestedModel, wireModel) {
			return continuityRoute{}, errors.New("session_continuity_unavailable")
		}
		return continuityRoute{session: session, wireModel: wireModel}, nil
	}

	// A fresh user turn may carry a long completed history. Recover both the
	// provider session and its last verified wire model from the newest surviving
	// router handle. Missing old records are tolerated.
	for i := currentStart - 1; i >= 0; i-- {
		item, _ := input[i].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			continue
		}
		id, _ := item["call_id"].(string)
		if !strings.HasPrefix(id, "call_ag_") {
			continue
		}
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		record, ok := c.replay().LookupCallIdentity(id, name, args)
		if !ok {
			continue
		}
		if explicitSession != "" && record.session != explicitSession {
			continue
		}
		if !sameContinuityModelFamily(requestedModel, record.model) {
			return continuityRoute{}, errors.New("session_continuity_unavailable")
		}
		return continuityRoute{session: record.session, wireModel: record.model}, nil
	}

	if explicitSession != "" {
		return continuityRoute{session: explicitSession, wireModel: requestedModel}, nil
	}
	return continuityRoute{}, errors.New("session_continuity_unavailable")
}

func sameContinuityModelFamily(requested, stored string) bool {
	return continuityModelFamily(requested) == continuityModelFamily(stored)
}

func continuityModelFamily(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range []string{"-low", "-medium", "-high"} {
		if strings.HasPrefix(model, "gemini-3.8-flash-") && strings.HasSuffix(model, suffix) {
			return "gemini-3.8-flash"
		}
	}
	switch model {
	case "gemini-3.1-pro-low", "gemini-pro-agent":
		return "gemini-3.1-pro"
	}
	return model
}

func hasToolHistory(payload map[string]any) bool {
	input, _ := payload["input"].([]any)
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] == "function_call" || item["type"] == "function_call_output" {
			return true
		}
	}
	return false
}

func stableSessionID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	value := binary.BigEndian.Uint64(digest[:8]) & 0x7fffffffffffffff
	return fmt.Sprintf("-%d", value)
}
