package antigravity

import (
	"crypto/sha256"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultReplayEntries = 1024
	defaultReplayTTL     = 30 * time.Minute
)

var foreignSignaturePrefix = regexp.MustCompile(`(?i)^(fc|ctc|tsc|call|msg|rs|resp|reasoning|item|ws|toolu|tool|func|function)[_-]`)
var signatureAlphabet = regexp.MustCompile(`^[A-Za-z0-9+/_=-]+$`)

type replayEntry struct {
	signature string
	expiresAt time.Time
	usedAt    time.Time
}

type ReplayCache struct {
	mu      sync.Mutex
	entries map[string]replayEntry
	max     int
	ttl     time.Duration
	now     func() time.Time
	calls   map[string]callRecord
}

type callRecord struct {
	profile, model, session, name, wireID, signature string
	args                                             [32]byte
	expiresAt, usedAt                                time.Time
}

func NewReplayCache(max int, ttl time.Duration) *ReplayCache {
	if max <= 0 {
		max = defaultReplayEntries
	}
	if ttl <= 0 {
		ttl = defaultReplayTTL
	}
	return &ReplayCache{entries: make(map[string]replayEntry), calls: make(map[string]callRecord), max: max, ttl: ttl, now: time.Now}
}

// Opaque local call handles let headerless clients (including Qwen) resume the
// exact session without identifying it by prompt text. Wire IDs remain private.
func (c *ReplayCache) RememberCall(id, wireID, profile, model, session, name string, args any, signature string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowTime()
	c.sweepLocked(now)
	if len(c.calls) >= c.max {
		oldest := ""
		for k, v := range c.calls {
			if oldest == "" || v.usedAt.Before(c.calls[oldest].usedAt) {
				oldest = k
			}
		}
		delete(c.calls, oldest)
	}
	if !isLikelyRealThoughtSignature(signature) {
		signature = ""
	}
	c.calls[id] = callRecord{profile: profile, model: model, session: session, name: name, wireID: wireID, signature: signature, args: sha256.Sum256([]byte(canonicalArgs(args))), expiresAt: now.Add(c.ttl), usedAt: now}
}

func (c *ReplayCache) LookupCall(id, model, name string, args any) (callRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowTime()
	record, ok := c.calls[id]
	if !ok {
		return callRecord{}, false
	}
	if !record.expiresAt.After(now) {
		delete(c.calls, id)
		return callRecord{}, false
	}
	if record.model != model || record.name != name || record.args != sha256.Sum256([]byte(canonicalArgs(args))) {
		return callRecord{}, false
	}
	record.usedAt = now
	c.calls[id] = record
	return record, true
}

func (c *ReplayCache) Lookup(profileID, model, session, name string, args any) (string, bool) {
	if c == nil {
		return "", false
	}
	key := replayKey(profileID, model, session, name, args)
	now := c.nowTime()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !entry.expiresAt.After(now) {
		delete(c.entries, key)
		return "", false
	}
	entry.usedAt = now
	c.entries[key] = entry
	return entry.signature, true
}

func (c *ReplayCache) Remember(profileID, model, session, name string, args any, signature string) bool {
	if c == nil || !isLikelyRealThoughtSignature(signature) {
		return false
	}
	now := c.nowTime()
	key := replayKey(profileID, model, session, name, args)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.max {
		var oldestKey string
		var oldest time.Time
		for candidate, entry := range c.entries {
			if oldestKey == "" || entry.usedAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.usedAt
			}
		}
		if oldestKey != "" {
			delete(c.entries, oldestKey)
		}
	}
	c.entries[key] = replayEntry{signature: signature, expiresAt: now.Add(c.ttl), usedAt: now}
	return true
}

func (c *ReplayCache) ClearSession(profileID, model, session string) {
	if c == nil {
		return
	}
	prefix := strings.Join([]string{profileID, model, session}, "\x00") + "\x00"
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if strings.HasPrefix(key, prefix) {
			delete(c.entries, key)
		}
	}
	for id, record := range c.calls {
		if record.profile == profileID && record.model == model && record.session == session {
			delete(c.calls, id)
		}
	}
}

func (c *ReplayCache) sweepLocked(now time.Time) {
	for id, record := range c.calls {
		if !record.expiresAt.After(now) {
			delete(c.calls, id)
		}
	}
	for key, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, key)
		}
	}
}

func (c *ReplayCache) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func replayKey(profileID, model, session, name string, args any) string {
	return strings.Join([]string{profileID, model, session, name, canonicalArgs(args)}, "\x00")
}

func canonicalArgs(args any) string {
	if text, ok := args.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			args = decoded
		} else {
			return text
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func isLikelyRealThoughtSignature(signature string) bool {
	signature = strings.TrimSpace(signature)
	if len(signature) < 16 || signature == "skip_thought_signature_validator" {
		return false
	}
	if foreignSignaturePrefix.MatchString(signature) {
		return false
	}
	return signatureAlphabet.MatchString(signature)
}
