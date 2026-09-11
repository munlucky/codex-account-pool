package observability

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggerWritesStructuredEventWithoutSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var out bytes.Buffer
	logger, err := NewLogger(LoggerConfig{Out: &out, FilePath: path, DailyDir: filepath.Join(dir, "daily"), ServiceVersion: "v1", ServiceCommit: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	logger.Emit(Event{EventType: EventRequestEnd, RequestID: "r_test", Method: "POST", RouteTemplate: "/backend-api/codex/responses", ProfileRef: "p_deadbeef", StatusCode: 200, Outcome: OutcomeBodyEOF})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, out.Bytes()) {
		t.Fatalf("stdout and file differ: stdout=%q file=%q", out.Bytes(), data)
	}
	var event Event
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
		t.Fatal(err)
	}
	if event.SchemaVersion != SchemaVersion || event.ServiceVersion != "v1" || event.ServiceCommit != "abc" || event.EventType != EventRequestEnd {
		t.Fatalf("event=%+v", event)
	}
	for _, forbidden := range []string{"Authorization", "Bearer ", "access_token", "refresh_token", "cookie", "?"} {
		if strings.Contains(strings.ToLower(string(data)), strings.ToLower(forbidden)) {
			t.Fatalf("forbidden material %q in log: %s", forbidden, data)
		}
	}
}

func TestContextFieldsRemainOptionalForLegacyEvents(t *testing.T) {
	data, err := json.Marshal(Event{EventType: EventRequestEnd, RequestID: "r_legacy", StatusCode: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"context_analysis", "lineage_source", "body_ref", "request_bytes", "context_bytes", "tool_definition_bytes", "developer_bytes", "reasoning_bytes", "metadata_bytes", "input_tokens", "usage_available"} {
		if bytes.Contains(data, []byte(field)) {
			t.Fatalf("legacy event unexpectedly contains optional context field %q: %s", field, data)
		}
	}
}

func TestDailySummaryAggregatesContextWithoutRawContent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	logger, err := NewLogger(LoggerConfig{Out: bytes.NewBuffer(nil), DailyDir: filepath.Join(dir, "daily"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	logger.Emit(Event{
		EventType: EventRequestEnd, RequestID: "r_context", Method: "POST", RouteTemplate: "/backend-api/codex/responses", StatusCode: 200,
		ContextAnalysis: "analyzed", LineageSource: "prompt_cache_key", BodyRef: "b_opaque", RequestBytes: 60000, ContextBytes: 100000,
		InstructionsBytes: 10000, UserBytes: 5000, AssistantBytes: 15000, ToolOutputBytes: 30000, ToolDefinitionBytes: 20000,
		SystemBytes: 3000, DeveloperBytes: 4000, ReasoningBytes: 5000, MetadataBytes: 3000, OtherInputBytes: 5000,
		ContextDeltaAvailable: true, ContextGrowthBytes: 5000, ContextReuseRatio: 0.9, ContextAmplificationRatio: 10,
		UsageAvailable: true, InputTokens: 72000, CachedInputTokens: 61000, OutputTokens: 1200, ReasoningTokens: 700, TotalTokens: 73200,
	})
	logger.Emit(Event{
		EventType: EventRequestEnd, RequestID: "r_skipped", Method: "POST", RouteTemplate: "/backend-api/codex/responses", StatusCode: 200,
		ContextAnalysis: "skipped", ContextSkipReason: "malformed_json", RequestBytes: 17,
	})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "daily", "2026-09-10.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary dailySummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Context == nil || summary.Context.Analyzed != 1 || summary.Context.Skipped != 1 || summary.Context.SkipReasons["malformed_json"] != 1 {
		t.Fatalf("context daily summary=%+v", summary.Context)
	}
	if summary.Context.Metrics["request_bytes"].Count != 2 || summary.Context.Metrics["context_bytes"].Count != 1 || summary.Context.Metrics["context_reuse_ratio"].Count != 1 {
		t.Fatalf("context distributions=%+v", summary.Context.Metrics)
	}
	if summary.Context.LineageSources["prompt_cache_key"] != 1 || summary.Context.ToolDefinitionBytes != 20000 || summary.Context.SystemBytes != 3000 || summary.Context.DeveloperBytes != 4000 || summary.Context.ReasoningBytes != 5000 || summary.Context.MetadataBytes != 3000 {
		t.Fatalf("context lineage/composition summary=%+v", summary.Context)
	}
	if summary.Context.UsageRequests != 1 || summary.Context.InputTokens != 72000 || summary.Context.CachedInputTokens != 61000 {
		t.Fatalf("context token totals=%+v", summary.Context)
	}
	if strings.Contains(string(data), "SUPER_SECRET") || strings.Contains(string(data), "PRIVATE_SOURCE_TEXT") {
		t.Fatalf("daily summary contains raw content: %s", data)
	}
}

func TestLoggerRotatesAndPersistsDailySummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	logger, err := NewLogger(LoggerConfig{Out: bytes.NewBuffer(nil), FilePath: path, DailyDir: filepath.Join(dir, "daily"), MaxBytes: 250, MaxFiles: 3, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		logger.Emit(Event{EventType: EventRequestEnd, RequestID: "r_rotation_test", RouteTemplate: "/backend-api/codex/responses", Transport: "http", StatusCode: 200, Outcome: OutcomeBodyEOF, GatewayTotalMS: 10})
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated log: %v", err)
	}
	dailyPath := filepath.Join(dir, "daily", "2026-09-10.json")
	data, err := os.ReadFile(dailyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"requests": 8`) {
		t.Fatalf("daily summary=%s", data)
	}
}
