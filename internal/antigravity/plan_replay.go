package antigravity

import (
	"encoding/json"
	"strings"
)

// Qwen replaces only the plan argument after approval and on resume. Never
// follow the client-supplied path or send that replacement with a signature
// bound to the original arguments. All other fields must still match exactly.
func matchesCallArgs(record callRecord, args any) bool {
	if record.args == argsDigest(args) {
		return true
	}
	if record.name != "exit_plan_mode" || len(record.originalArgs) == 0 {
		return false
	}
	var received, original map[string]any
	if json.Unmarshal([]byte(canonicalArgs(args)), &received) != nil ||
		json.Unmarshal(record.originalArgs, &original) != nil || received == nil || original == nil {
		return false
	}
	plan, ok := received["plan"].(string)
	const prefix = "[Plan approved and saved to "
	const suffix = ". The plan text was removed from the conversation after approval; read that file if you need to consult it again.]"
	if !ok || !strings.HasPrefix(plan, prefix) || !strings.HasSuffix(plan, suffix) || len(plan) <= len(prefix)+len(suffix) {
		return false
	}
	if _, ok := original["plan"].(string); !ok {
		return false
	}
	received["plan"] = original["plan"]
	return record.args == argsDigest(received)
}
