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

func (c *Client) resolveSession(r *http.Request, payload map[string]any, model string) (string, error) {
	if id := explicitSessionID(r, payload); id != "" {
		return id, nil
	}
	if !hasToolHistory(payload) {
		return sessionID(r, payload), nil
	}
	input, _ := payload["input"].([]any)
	session := ""
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		id, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		record, ok := c.replay().LookupCall(id, model, name, args)
		if !ok || (session != "" && record.session != session) {
			return "", errors.New("session_continuity_unavailable")
		}
		session = record.session
	}
	if session == "" {
		return "", errors.New("session_continuity_unavailable")
	}
	return session, nil
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
