package antigravity

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultReplayEntries   = 1024
	defaultReplayTTL       = 7 * 24 * time.Hour
	defaultReplayRetention = 30 * 24 * time.Hour
	continuityStateVersion = 1
)

var foreignSignaturePrefix = regexp.MustCompile("(?i)^(fc|ctc|tsc|call|msg|rs|resp|reasoning|item|ws|toolu|tool|func|function)[_-]")
var signatureAlphabet = regexp.MustCompile("^[A-Za-z0-9+/_=-]+$")

type replayEntry struct {
	signature                     string
	profile, model, session, name string
	args                          [32]byte
	createdAt, expiresAt, usedAt  time.Time
}

type callRecord struct {
	originalArgs                                     json.RawMessage
	profile, model, session, name, wireID, signature string
	step                                             string
	position                                         int
	args                                             [32]byte
	createdAt, expiresAt, usedAt                     time.Time
}

type sessionRecord struct {
	profile, model, session      string
	createdAt, expiresAt, usedAt time.Time
}

type ReplayCache struct {
	mu        sync.Mutex
	entries   map[string]replayEntry
	calls     map[string]callRecord
	sessions  map[string]sessionRecord
	max       int
	ttl       time.Duration
	retention time.Duration
	path      string
	now       func() time.Time
}

type persistedContinuityState struct {
	Version  int
	Entries  []persistedReplayEntry
	Calls    []persistedCallRecord
	Sessions []persistedSessionRecord
}

type persistedReplayEntry struct {
	Key                                      string
	Signature, Profile, Model, Session, Name string
	Args                                     [32]byte
	CreatedAt, ExpiresAt, UsedAt             time.Time
}

type persistedCallRecord struct {
	OriginalArgs                                             json.RawMessage `json:",omitempty"`
	ID                                                       string
	Profile, Model, Session, Name, WireID, Signature, StepID string
	Position                                                 int
	Args                                                     [32]byte
	CreatedAt, ExpiresAt, UsedAt                             time.Time
}

type persistedSessionRecord struct {
	Profile, Model, Session      string
	CreatedAt, ExpiresAt, UsedAt time.Time
}

func NewReplayCache(max int, ttl time.Duration) *ReplayCache {
	return newReplayCache(max, ttl, defaultReplayRetention, "")
}

func NewPersistentReplayCache(max int, ttl, retention time.Duration, path string) (*ReplayCache, error) {
	cache := newReplayCache(max, ttl, retention, strings.TrimSpace(path))
	if cache.path == "" {
		return cache, nil
	}
	if err := cache.load(); err != nil {
		return nil, err
	}
	cache.mu.Lock()
	err := cache.persistLocked()
	cache.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return cache, nil
}

func newReplayCache(max int, ttl, retention time.Duration, path string) *ReplayCache {
	if max <= 0 {
		max = defaultReplayEntries
	}
	if ttl <= 0 {
		ttl = defaultReplayTTL
	}
	if retention <= 0 {
		retention = defaultReplayRetention
	}
	if retention < ttl {
		retention = ttl
	}
	return &ReplayCache{
		entries: make(map[string]replayEntry), calls: make(map[string]callRecord), sessions: make(map[string]sessionRecord),
		max: max, ttl: ttl, retention: retention, path: path, now: time.Now,
	}
}

// Opaque local call handles let headerless clients (including Qwen) resume the
// exact provider session without exposing provider wire IDs or thought signatures.
func (c *ReplayCache) RememberCall(id, wireID, profile, model, session, name string, args any, signature, step string, position int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowTime()
	c.sweepLocked(now)
	c.ensureCapacityLocked(sessionKey(model, session))
	if !isLikelyRealThoughtSignature(signature) {
		signature = ""
	}
	record := callRecord{
		profile: profile, model: model, session: session, name: name, wireID: wireID, signature: signature,
		step: step, position: position, args: argsDigest(args), createdAt: now, usedAt: now,
	}
	if name == "exit_plan_mode" {
		record.originalArgs = json.RawMessage(canonicalArgs(args))
	}
	record.expiresAt = c.extendExpiry(now, record.createdAt)
	c.calls[id] = record
	c.bindSessionLocked(profile, model, session, now)
	_ = c.persistLocked()
}

func (c *ReplayCache) LookupCall(id, model, name string, args any) (callRecord, bool) {
	record, ok := c.lookupCallIdentity(id, name, args)
	if !ok || record.model != model {
		return callRecord{}, false
	}
	return record, true
}

// LookupCallIdentity validates the opaque router call handle, function name,
// and canonical arguments without requiring the caller to already know the
// provider wire-model variant that created the call. The returned model is the
// continuity authority for that conversation.
func (c *ReplayCache) LookupCallIdentity(id, name string, args any) (callRecord, bool) {
	return c.lookupCallIdentity(id, name, args)
}

func (c *ReplayCache) lookupCallIdentity(id, name string, args any) (callRecord, bool) {
	if c == nil {
		return callRecord{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowTime()
	record, ok := c.calls[id]
	if !ok || !record.expiresAt.After(now) {
		if ok {
			delete(c.calls, id)
		}
		return callRecord{}, false
	}
	if record.name != name || !matchesCallArgs(record, args) {
		return callRecord{}, false
	}
	record.usedAt = now
	record.expiresAt = c.extendExpiry(now, record.createdAt)
	c.calls[id] = record
	c.touchSessionLocked(record.model, record.session, now)
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
	if !ok || !entry.expiresAt.After(now) {
		if ok {
			delete(c.entries, key)
		}
		return "", false
	}
	entry.usedAt = now
	entry.expiresAt = c.extendExpiry(now, entry.createdAt)
	c.entries[key] = entry
	c.touchSessionLocked(model, session, now)
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
	c.ensureCapacityLocked(sessionKey(model, session))
	entry := replayEntry{
		signature: signature, profile: profileID, model: model, session: session, name: name,
		args: argsDigest(args), createdAt: now, usedAt: now,
	}
	entry.expiresAt = c.extendExpiry(now, entry.createdAt)
	c.entries[key] = entry
	c.bindSessionLocked(profileID, model, session, now)
	_ = c.persistLocked()
	return true
}

// BindSession persists provider profile affinity independently from tool calls.
func (c *ReplayCache) BindSession(profileID, model, session string) bool {
	if c == nil || profileID == "" || model == "" || session == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowTime()
	c.sweepLocked(now)
	key := sessionKey(model, session)
	if existing, ok := c.sessions[key]; ok && existing.profile != profileID {
		hasCalls := false
		for _, call := range c.calls {
			if call.model == model && call.session == session {
				hasCalls = true
				break
			}
		}
		if hasCalls {
			return false
		}
		delete(c.sessions, key)
	}
	c.bindSessionLocked(profileID, model, session, now)
	_ = c.persistLocked()
	return true
}

func (c *ReplayCache) SessionProfile(model, session string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowTime()
	key := sessionKey(model, session)
	record, ok := c.sessions[key]
	if !ok || !record.expiresAt.After(now) {
		if ok {
			c.deleteSessionLocked(key)
		}
		return "", false
	}
	return record.profile, true
}

func (c *ReplayCache) InvalidateCall(id string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.calls, id)
	_ = c.persistLocked()
}

func (c *ReplayCache) ClearSession(profileID, model, session string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey(model, session)
	for k, entry := range c.entries {
		if entry.profile == profileID && entry.model == model && entry.session == session {
			delete(c.entries, k)
		}
	}
	for id, record := range c.calls {
		if record.profile == profileID && record.model == model && record.session == session {
			delete(c.calls, id)
		}
	}
	if record, ok := c.sessions[key]; ok && record.profile == profileID {
		delete(c.sessions, key)
	}
	_ = c.persistLocked()
}

func (c *ReplayCache) bindSessionLocked(profileID, model, session string, now time.Time) {
	if profileID == "" || model == "" || session == "" {
		return
	}
	key := sessionKey(model, session)
	record, ok := c.sessions[key]
	if !ok {
		record = sessionRecord{profile: profileID, model: model, session: session, createdAt: now}
	}
	if record.profile != profileID {
		return
	}
	record.usedAt = now
	record.expiresAt = c.extendExpiry(now, record.createdAt)
	c.sessions[key] = record
}

func (c *ReplayCache) touchSessionLocked(model, session string, now time.Time) {
	key := sessionKey(model, session)
	record, ok := c.sessions[key]
	if !ok {
		return
	}
	record.usedAt = now
	record.expiresAt = c.extendExpiry(now, record.createdAt)
	c.sessions[key] = record
}

func (c *ReplayCache) ensureCapacityLocked(currentSession string) {
	if len(c.calls) < c.max {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, record := range c.sessions {
		if key == currentSession {
			continue
		}
		if oldestKey == "" || record.usedAt.Before(oldest) {
			oldestKey, oldest = key, record.usedAt
		}
	}
	if oldestKey != "" {
		c.deleteSessionLocked(oldestKey)
	}
	// Never evict a single call from the active session. A large conversation
	// may temporarily exceed max; preserving session integrity is more important.
}

func (c *ReplayCache) deleteSessionLocked(key string) {
	delete(c.sessions, key)
	for id, call := range c.calls {
		if sessionKey(call.model, call.session) == key {
			delete(c.calls, id)
		}
	}
	for k, entry := range c.entries {
		if sessionKey(entry.model, entry.session) == key {
			delete(c.entries, k)
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
	for key, record := range c.sessions {
		if !record.expiresAt.After(now) {
			c.deleteSessionLocked(key)
		}
	}
}

func (c *ReplayCache) extendExpiry(now, createdAt time.Time) time.Time {
	idle := now.Add(c.ttl)
	absolute := createdAt.Add(c.retention)
	if absolute.Before(idle) {
		return absolute
	}
	return idle
}

func (c *ReplayCache) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func sessionKey(model, session string) string {
	return model + "\x00" + session
}

func replayKey(profileID, model, session, name string, args any) string {
	digest := argsDigest(args)
	return strings.Join([]string{profileID, model, session, name, fmt.Sprintf("%x", digest)}, "\x00")
}

func argsDigest(args any) [32]byte {
	return sha256.Sum256([]byte(canonicalArgs(args)))
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

func (c *ReplayCache) load() error {
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Antigravity continuity state: %w", err)
	}
	var state persistedContinuityState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode Antigravity continuity state: %w", err)
	}
	if state.Version != continuityStateVersion {
		return fmt.Errorf("unsupported Antigravity continuity state version %d", state.Version)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range state.Entries {
		c.entries[item.Key] = replayEntry{
			signature: item.Signature, profile: item.Profile, model: item.Model, session: item.Session, name: item.Name,
			args: item.Args, createdAt: item.CreatedAt, expiresAt: item.ExpiresAt, usedAt: item.UsedAt,
		}
	}
	for _, item := range state.Calls {
		if len(item.OriginalArgs) > 0 && (item.Name != "exit_plan_mode" || argsDigest(string(item.OriginalArgs)) != item.Args) {
			return fmt.Errorf("invalid persisted plan replay arguments")
		}
		c.calls[item.ID] = callRecord{
			originalArgs: item.OriginalArgs,
			profile:      item.Profile, model: item.Model, session: item.Session, name: item.Name, wireID: item.WireID,
			signature: item.Signature, step: item.StepID, position: item.Position, args: item.Args,
			createdAt: item.CreatedAt, expiresAt: item.ExpiresAt, usedAt: item.UsedAt,
		}
	}
	for _, item := range state.Sessions {
		c.sessions[sessionKey(item.Model, item.Session)] = sessionRecord{
			profile: item.Profile, model: item.Model, session: item.Session,
			createdAt: item.CreatedAt, expiresAt: item.ExpiresAt, usedAt: item.UsedAt,
		}
	}
	c.sweepLocked(c.nowTime())
	return nil
}

func (c *ReplayCache) persistLocked() error {
	if c.path == "" {
		return nil
	}
	state := persistedContinuityState{Version: continuityStateVersion}
	for key, entry := range c.entries {
		state.Entries = append(state.Entries, persistedReplayEntry{
			Key: key, Signature: entry.signature, Profile: entry.profile, Model: entry.model, Session: entry.session, Name: entry.name,
			Args: entry.args, CreatedAt: entry.createdAt, ExpiresAt: entry.expiresAt, UsedAt: entry.usedAt,
		})
	}
	for id, record := range c.calls {
		state.Calls = append(state.Calls, persistedCallRecord{
			OriginalArgs: record.originalArgs,
			ID:           id, Profile: record.profile, Model: record.model, Session: record.session, Name: record.name, WireID: record.wireID,
			Signature: record.signature, StepID: record.step, Position: record.position, Args: record.args,
			CreatedAt: record.createdAt, ExpiresAt: record.expiresAt, UsedAt: record.usedAt,
		})
	}
	for _, record := range c.sessions {
		state.Sessions = append(state.Sessions, persistedSessionRecord{
			Profile: record.profile, Model: record.model, Session: record.session,
			CreatedAt: record.createdAt, ExpiresAt: record.expiresAt, UsedAt: record.usedAt,
		})
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Antigravity continuity state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Antigravity continuity directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".continuity-*.tmp")
	if err != nil {
		return fmt.Errorf("create Antigravity continuity temp file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure Antigravity continuity temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write Antigravity continuity state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync Antigravity continuity state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close Antigravity continuity state: %w", err)
	}
	if err := os.Rename(name, c.path); err != nil {
		return fmt.Errorf("replace Antigravity continuity state: %w", err)
	}
	return nil
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
