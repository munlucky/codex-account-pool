package contextobs

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const fingerprintBytes = 6

// Analyzer derives structural metadata from request bytes without mutating them.
type Analyzer struct {
	key      []byte
	maxBytes int
}

func NewProcessSecret() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("contextobs secret: %w", err)
	}
	return key, nil
}

func NewAnalyzer(key []byte, maxBytes int) (*Analyzer, error) {
	if len(key) == 0 {
		return nil, errors.New("contextobs analyzer requires a process-local key")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxAnalyzedBodyBytes
	}
	return &Analyzer{key: append([]byte(nil), key...), maxBytes: maxBytes}, nil
}

// AnalyzeRequest analyzes an unencoded request body. It is retained for tests
// and callers that already know the bytes are the JSON representation.
func (a *Analyzer) AnalyzeRequest(body []byte) RequestMetrics {
	return a.AnalyzeRequestEncoded(body, "")
}

// AnalyzeRequestEncoded derives metadata from the request representation while
// preserving the exact wire bytes. Decoding happens only in this observer path;
// callers continue proxying the original body regardless of analysis outcome.
func (a *Analyzer) AnalyzeRequestEncoded(body []byte, contentEncoding string) RequestMetrics {
	m := RequestMetrics{
		RequestBytes:   int64(len(body)),
		BodyRef:        a.fingerprint("b_", body),
		AnalysisStatus: AnalysisSkipped,
	}
	if len(body) > a.maxBytes {
		m.SkipReason = SkipOversized
		return m
	}

	decoded, skipReason := a.decodeForAnalysis(body, contentEncoding)
	if skipReason != "" {
		m.SkipReason = skipReason
		return m
	}
	m.ContextBytes = int64(len(decoded))
	if len(decoded) > a.maxBytes {
		m.SkipReason = SkipOversized
		return m
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &top); err != nil {
		m.SkipReason = SkipMalformed
		return m
	}
	if top == nil {
		m.SkipReason = SkipUnsupported
		return m
	}

	m.AnalysisStatus = AnalysisAnalyzed

	if raw := meaningfulRaw(top["instructions"]); raw != nil {
		m.InstructionsBytes = int64(len(raw))
		m.ItemRefs = append(m.ItemRefs, ItemRef{
			Type: "instructions",
			Size: int64(len(raw)),
			Ref:  a.fingerprint("i_", raw),
		})
	}

	if raw := top["input"]; len(raw) > 0 {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err == nil {
			for _, item := range items {
				a.addInputItem(&m, item)
			}
		}
	}

	// Tool definitions can be large and repeated. Treat them as opaque reuse
	// items, but never confuse definitions with observed tool outputs.
	if raw := top["tools"]; len(raw) > 0 {
		var tools []json.RawMessage
		if err := json.Unmarshal(raw, &tools); err == nil {
			for _, tool := range tools {
				if rawTool := meaningfulRaw(tool); rawTool != nil {
					m.ToolDefinitionBytes += int64(len(rawTool))
					m.ItemRefs = append(m.ItemRefs, ItemRef{
						Type: "tool_definition",
						Size: int64(len(rawTool)),
						Ref:  a.fingerprint("i_", rawTool),
					})
				}
			}
		}
	}

	if raw := meaningfulRaw(top["metadata"]); raw != nil {
		m.MetadataBytes = int64(len(raw))
	}

	known := m.InstructionsBytes + m.UserBytes + m.AssistantBytes + m.ToolOutputBytes +
		m.ToolDefinitionBytes + m.SystemBytes + m.DeveloperBytes + m.ReasoningBytes + m.MetadataBytes
	m.OtherInputBytes = m.ContextBytes - known
	if m.OtherInputBytes < 0 {
		m.OtherInputBytes = 0
	}

	m.LineageRef, m.LineageSource = a.extractStableLineage(top)
	m.PreviousResponseRef = a.extractStringRef(top["previous_response_id"], "r_")
	return m
}

func (a *Analyzer) decodeForAnalysis(body []byte, contentEncoding string) ([]byte, string) {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	switch encoding {
	case "", "identity":
		return body, ""
	case "zstd":
		decoder, err := zstd.NewReader(bytes.NewReader(body),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(uint64(a.maxBytes)),
		)
		if err != nil {
			return nil, SkipDecodeError
		}
		defer decoder.Close()
		decoded, err := io.ReadAll(io.LimitReader(decoder, int64(a.maxBytes)+1))
		if err != nil {
			return nil, SkipDecodeError
		}
		if len(decoded) > a.maxBytes {
			return nil, SkipOversized
		}
		return decoded, ""
	default:
		return nil, SkipUnsupportedEncoding
	}
}

func (a *Analyzer) addInputItem(m *RequestMetrics, raw json.RawMessage) {
	raw = meaningfulRaw(raw)
	if raw == nil {
		return
	}
	m.InputItems++

	var meta struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	_ = json.Unmarshal(raw, &meta)
	kind := classifyItem(meta.Type, meta.Role)
	sz := int64(len(raw))
	switch kind {
	case "user":
		m.UserItems++
		m.UserBytes += sz
	case "assistant":
		m.AssistantItems++
		m.AssistantBytes += sz
	case "tool_output":
		m.ToolItems++
		m.ToolOutputBytes += sz
	case "system":
		m.SystemBytes += sz
	case "developer":
		m.DeveloperBytes += sz
	case "reasoning":
		m.ReasoningBytes += sz
	}
	m.ItemRefs = append(m.ItemRefs, ItemRef{
		Type: kind,
		Size: sz,
		Ref:  a.fingerprint("i_", raw),
	})
}

func classifyItem(itemType, role string) string {
	t := strings.ToLower(strings.TrimSpace(itemType))
	r := strings.ToLower(strings.TrimSpace(role))
	if t == "function_call_output" || t == "tool_output" || strings.HasSuffix(t, "_call_output") || r == "tool" {
		return "tool_output"
	}
	if t == "reasoning" || strings.HasPrefix(t, "reasoning_") || strings.HasSuffix(t, "_reasoning") {
		return "reasoning"
	}
	switch r {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "system":
		return "system"
	case "developer":
		return "developer"
	default:
		return "other"
	}
}

func meaningfulRaw(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return raw
}

func (a *Analyzer) extractStableLineage(top map[string]json.RawMessage) (string, string) {
	for _, key := range []string{"conversation_id", "thread_id"} {
		if ref := a.extractStringRef(top[key], "l_"); ref != "" {
			return ref, key
		}
	}
	if raw := top["conversation"]; len(raw) > 0 {
		if ref := a.extractStringRef(raw, "l_"); ref != "" {
			return ref, "conversation"
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			if ref := a.extractStringRef(obj["id"], "l_"); ref != "" {
				return ref, "conversation_id"
			}
		}
	}
	// Codex uses prompt_cache_key as a stable session/cache affinity hint. It is
	// safe for lineage only after conversion to the same process-local HMAC used
	// for other opaque references; the raw key is never retained or emitted.
	if ref := a.extractStringRef(top["prompt_cache_key"], "l_"); ref != "" {
		return ref, "prompt_cache_key"
	}
	return "", ""
}

func (a *Analyzer) extractStringRef(raw json.RawMessage, prefix string) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || strings.TrimSpace(s) == "" {
		return ""
	}
	return a.fingerprint(prefix, []byte(s))
}

func (a *Analyzer) fingerprint(prefix string, data []byte) string {
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write(data)
	sum := mac.Sum(nil)
	return prefix + hex.EncodeToString(sum[:fingerprintBytes])
}
