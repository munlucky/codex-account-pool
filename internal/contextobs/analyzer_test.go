package contextobs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestAnalyzeRequestCollectsOnlyStructuralMetadata(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6",
		"conversation":{"id":"conv-private-123"},
		"previous_response_id":"resp-private-456",
		"instructions":"SUPER_SECRET_PROMPT_938482",
		"input":[
			{"type":"message","role":"user","content":"PRIVATE_USER_TEXT"},
			{"type":"message","role":"assistant","content":"PRIVATE_ASSISTANT_TEXT"},
			{"type":"function_call_output","call_id":"c1","output":"PRIVATE_TOOL_OUTPUT"},
			{"type":"message","role":"developer","content":"PRIVATE_DEVELOPER_TEXT"}
		],
		"tools":[{"type":"function","name":"private_tool_name","description":"PRIVATE_TOOL_DESCRIPTION"}]
	}`)
	original := append([]byte(nil), body...)
	analyzer, err := NewAnalyzer([]byte("deterministic-test-process-key"), 0)
	if err != nil {
		t.Fatal(err)
	}

	m := analyzer.AnalyzeRequest(body)
	if !bytes.Equal(body, original) {
		t.Fatal("analysis mutated request bytes")
	}
	if m.AnalysisStatus != AnalysisAnalyzed || m.SkipReason != "" {
		t.Fatalf("analysis status=%q reason=%q", m.AnalysisStatus, m.SkipReason)
	}
	if m.RequestBytes != int64(len(body)) {
		t.Fatalf("request bytes=%d want=%d", m.RequestBytes, len(body))
	}
	if m.InputItems != 4 || m.UserItems != 1 || m.AssistantItems != 1 || m.ToolItems != 1 {
		t.Fatalf("unexpected item counts: %+v", m)
	}
	if m.InstructionsBytes == 0 || m.UserBytes == 0 || m.AssistantBytes == 0 || m.ToolOutputBytes == 0 || m.OtherInputBytes == 0 {
		t.Fatalf("expected non-zero composition: %+v", m)
	}
	if m.BodyRef == "" || m.LineageRef == "" || m.PreviousResponseRef == "" {
		t.Fatalf("expected opaque refs: %+v", m)
	}
	if len(m.ItemRefs) != 6 { // instructions + 4 input items + tool definition
		t.Fatalf("item refs=%d want=6", len(m.ItemRefs))
	}

	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"SUPER_SECRET_PROMPT_938482",
		"PRIVATE_USER_TEXT",
		"PRIVATE_ASSISTANT_TEXT",
		"PRIVATE_TOOL_OUTPUT",
		"PRIVATE_DEVELOPER_TEXT",
		"PRIVATE_TOOL_DESCRIPTION",
		"conv-private-123",
		"resp-private-456",
	} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("serialized metrics leaked %q: %s", secret, encoded)
		}
	}
	for _, item := range m.ItemRefs {
		if !strings.HasPrefix(item.Ref, "i_") || len(item.Ref) != len("i_")+fingerprintBytes*2 {
			t.Fatalf("unexpected item ref %q", item.Ref)
		}
	}
}

func TestAnalyzeRequestFingerprintIsProcessLocalAndDeterministic(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":"same"}]}`)
	a, _ := NewAnalyzer([]byte("key-a"), 0)
	b, _ := NewAnalyzer([]byte("key-b"), 0)
	m1 := a.AnalyzeRequest(body)
	m2 := a.AnalyzeRequest(body)
	m3 := b.AnalyzeRequest(body)
	if m1.BodyRef != m2.BodyRef || m1.ItemRefs[0].Ref != m2.ItemRefs[0].Ref {
		t.Fatal("same process key and content must produce the same opaque ref")
	}
	if m1.BodyRef == m3.BodyRef || m1.ItemRefs[0].Ref == m3.ItemRefs[0].Ref {
		t.Fatal("different process keys must not produce stable cross-process refs")
	}
}

func TestAnalyzeRequestFailOpenCoverageStates(t *testing.T) {
	a, _ := NewAnalyzer([]byte("key"), 16)
	malformed := a.AnalyzeRequest([]byte(`{"input":`))
	if malformed.AnalysisStatus != AnalysisSkipped || malformed.SkipReason != SkipMalformed {
		t.Fatalf("malformed=%+v", malformed)
	}
	over := a.AnalyzeRequest([]byte(strings.Repeat("x", 17)))
	if over.AnalysisStatus != AnalysisSkipped || over.SkipReason != SkipOversized {
		t.Fatalf("oversized=%+v", over)
	}
	unsupported := a.AnalyzeRequest([]byte(`null`))
	if unsupported.AnalysisStatus != AnalysisSkipped || unsupported.SkipReason != SkipUnsupported {
		t.Fatalf("unsupported=%+v", unsupported)
	}
	if malformed.RequestBytes == 0 || over.RequestBytes != 17 || malformed.BodyRef == "" || over.BodyRef == "" {
		t.Fatal("base request size/fingerprint must remain available even when structural analysis is skipped")
	}
}

func TestAnalyzeRequestEncodedZstdKeepsWireAndContextSizesSeparate(t *testing.T) {
	payload := []byte(`{"conversation_id":"conv-zstd","instructions":"PRIVATE_INSTRUCTION","input":[{"type":"message","role":"user","content":"PRIVATE_USER_TEXT"}]}`)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(payload, nil)
	encoder.Close()
	original := append([]byte(nil), compressed...)

	a, _ := NewAnalyzer([]byte("zstd-observer-key"), 0)
	m := a.AnalyzeRequestEncoded(compressed, "zstd")
	if !bytes.Equal(compressed, original) {
		t.Fatal("zstd observation mutated wire bytes")
	}
	if m.AnalysisStatus != AnalysisAnalyzed || m.SkipReason != "" {
		t.Fatalf("zstd analysis=%+v", m)
	}
	if m.RequestBytes != int64(len(compressed)) || m.ContextBytes != int64(len(payload)) {
		t.Fatalf("sizes wire=%d/%d context=%d/%d", m.RequestBytes, len(compressed), m.ContextBytes, len(payload))
	}
	if m.UserItems != 1 || m.InstructionsBytes == 0 || m.OtherInputBytes <= 0 {
		t.Fatalf("composition not decoded: %+v", m)
	}
	encoded, _ := json.Marshal(m)
	for _, secret := range []string{"PRIVATE_INSTRUCTION", "PRIVATE_USER_TEXT", "conv-zstd"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("serialized zstd metrics leaked %q: %s", secret, encoded)
		}
	}
}

func TestAnalyzeRequestEncodedFailsOpenForBadOrUnsupportedEncoding(t *testing.T) {
	a, _ := NewAnalyzer([]byte("zstd-fail-open-key"), 0)
	bad := a.AnalyzeRequestEncoded([]byte("not-zstd"), "zstd")
	if bad.AnalysisStatus != AnalysisSkipped || bad.SkipReason != SkipDecodeError || bad.RequestBytes != int64(len("not-zstd")) {
		t.Fatalf("bad zstd=%+v", bad)
	}
	unsupported := a.AnalyzeRequestEncoded([]byte(`{"input":[]}`), "br")
	if unsupported.AnalysisStatus != AnalysisSkipped || unsupported.SkipReason != SkipUnsupportedEncoding {
		t.Fatalf("unsupported encoding=%+v", unsupported)
	}
}

func TestAnalyzeRequestUsesPromptCacheKeyAsOpaqueLineageAndSplitsComposition(t *testing.T) {
	body := []byte(`{
		"prompt_cache_key":"PRIVATE_PROMPT_CACHE_KEY_123",
		"instructions":"PRIVATE_INSTRUCTION",
		"metadata":{"private":"PRIVATE_METADATA"},
		"input":[
			{"type":"message","role":"system","content":"PRIVATE_SYSTEM"},
			{"type":"message","role":"developer","content":"PRIVATE_DEVELOPER"},
			{"type":"reasoning","encrypted_content":"PRIVATE_ENCRYPTED_REASONING"},
			{"type":"message","role":"user","content":"PRIVATE_USER"},
			{"type":"message","role":"assistant","content":"PRIVATE_ASSISTANT"},
			{"type":"function_call_output","output":"PRIVATE_TOOL_OUTPUT"}
		],
		"tools":[{"type":"function","name":"private_tool","description":"PRIVATE_TOOL_DEFINITION"}]
	}`)
	a, _ := NewAnalyzer([]byte("prompt-cache-lineage-key"), 0)
	m := a.AnalyzeRequest(body)

	if m.AnalysisStatus != AnalysisAnalyzed || m.LineageRef == "" || m.LineageSource != "prompt_cache_key" {
		t.Fatalf("prompt-cache lineage not captured safely: %+v", m)
	}
	if m.ToolDefinitionBytes <= 0 || m.SystemBytes <= 0 || m.DeveloperBytes <= 0 || m.ReasoningBytes <= 0 || m.MetadataBytes <= 0 {
		t.Fatalf("expected structural composition categories: %+v", m)
	}
	categorized := m.InstructionsBytes + m.UserBytes + m.AssistantBytes + m.ToolOutputBytes +
		m.ToolDefinitionBytes + m.SystemBytes + m.DeveloperBytes + m.ReasoningBytes + m.MetadataBytes + m.OtherInputBytes
	if categorized != m.ContextBytes {
		t.Fatalf("composition must partition decoded context: categorized=%d context=%d metrics=%+v", categorized, m.ContextBytes, m)
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"PRIVATE_PROMPT_CACHE_KEY_123", "PRIVATE_INSTRUCTION", "PRIVATE_METADATA", "PRIVATE_SYSTEM",
		"PRIVATE_DEVELOPER", "PRIVATE_ENCRYPTED_REASONING", "PRIVATE_USER", "PRIVATE_ASSISTANT",
		"PRIVATE_TOOL_OUTPUT", "PRIVATE_TOOL_DEFINITION",
	} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("serialized structural metrics leaked %q: %s", secret, encoded)
		}
	}
}

func TestExplicitLineageIdentifiersTakePrecedenceOverPromptCacheKey(t *testing.T) {
	a, _ := NewAnalyzer([]byte("lineage-precedence-key"), 0)
	m := a.AnalyzeRequest([]byte(`{"thread_id":"thread-private","prompt_cache_key":"cache-private","input":[]}`))
	if m.LineageSource != "thread_id" {
		t.Fatalf("lineage source=%q want thread_id", m.LineageSource)
	}
	if m.LineageRef != a.fingerprint("l_", []byte("thread-private")) {
		t.Fatalf("unexpected lineage ref %q", m.LineageRef)
	}
}
