package antigravity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
)

func sessionID(r *http.Request, payload map[string]any) string {
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
	if value := firstUserText(payload); value != "" {
		return stableSessionID("user:" + value)
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		value := binary.BigEndian.Uint64(raw[:]) & 0x7fffffffffffffff
		return fmt.Sprintf("-%d", value)
	}
	return "-1"
}

func stableSessionID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	value := binary.BigEndian.Uint64(digest[:8]) & 0x7fffffffffffffff
	return fmt.Sprintf("-%d", value)
}

func firstUserText(payload map[string]any) string {
	input, _ := payload["input"].([]any)
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item == nil || item["type"] != "message" || item["role"] != "user" {
			continue
		}
		switch content := item["content"].(type) {
		case string:
			if strings.TrimSpace(content) != "" {
				return content
			}
		case []any:
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if part == nil {
					continue
				}
				if typ, _ := part["type"].(string); typ == "input_text" || typ == "output_text" || typ == "text" {
					if text, _ := part["text"].(string); strings.TrimSpace(text) != "" {
						return text
					}
				}
			}
		}
	}
	return ""
}
