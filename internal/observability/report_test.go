package observability

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestReportSeparatesHTTPStatusFromStreamOutcomeAndCoverage(t *testing.T) {
	base := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	input := strings.Join([]string{
		`{"ts":"2026-09-10T15:00:01Z","schema_version":1,"event_type":"request_start","request_id":"r_ok"}`,
		`{"ts":"2026-09-10T15:00:02Z","schema_version":1,"event_type":"request_end","request_id":"r_ok","route_template":"/backend-api/codex/responses","transport":"http","status_code":200,"status_origin":"upstream","outcome":"body_eof","semantic_outcome":"completed","gateway_total_ms":100,"first_body_ms":20}`,
		`{"ts":"2026-09-10T15:00:03Z","schema_version":1,"event_type":"request_start","request_id":"r_broken"}`,
		`{"ts":"2026-09-10T15:00:04Z","schema_version":1,"event_type":"request_end","request_id":"r_broken","route_template":"/backend-api/codex/responses","transport":"http","status_code":200,"status_origin":"upstream","outcome":"upstream_read_error","error_category":"upstream_read","semantic_outcome":"unobserved","gateway_total_ms":200,"first_body_ms":30}`,
		`{"ts":"2026-09-10T15:00:05Z","schema_version":1,"event_type":"request_start","request_id":"r_unfinished"}`,
		`{"ts":"2026-09-10T15:00:06Z","schema_version":1,"event_type":"logger_health","dropped_events":2}`,
		`not-json`,
	}, "\n")
	report := NewReport(ReportOptions{Start: base, End: base.Add(time.Hour), Location: time.UTC})
	if err := report.AddReader(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if report.StatusCounts[200] != 2 || report.Outcomes[OutcomeBodyEOF] != 1 || report.Outcomes[OutcomeUpstreamReadError] != 1 || report.Unfinished() != 1 || report.DroppedEvents != 2 || report.Malformed != 1 {
		t.Fatalf("report=%+v", report)
	}
	var out bytes.Buffer
	report.WriteText(&out)
	text := out.String()
	for _, want := range []string{"200=2", "upstream_read_error=1", "unfinished=1", "dropped_events=2", "Coverage warning"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}

func TestReportContextMetricsAndInference(t *testing.T) {
	base := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	input := strings.Join([]string{
		`{"ts":"2026-09-10T15:00:01Z","schema_version":1,"event_type":"request_start","request_id":"r_replay","method":"POST","route_template":"/backend-api/codex/responses"}`,
		`{"ts":"2026-09-10T15:00:02Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_replay","method":"POST","route_template":"/backend-api/codex/responses","attempt":1,"status_code":429}`,
		`{"ts":"2026-09-10T15:00:03Z","schema_version":1,"event_type":"account_switch","request_id":"r_replay","method":"POST","route_template":"/backend-api/codex/responses"}`,
		`{"ts":"2026-09-10T15:00:04Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_replay","method":"POST","route_template":"/backend-api/codex/responses","attempt":2,"status_code":200}`,
		`{"ts":"2026-09-10T15:00:05Z","schema_version":1,"event_type":"request_end","request_id":"r_replay","method":"POST","route_template":"/backend-api/codex/responses","status_code":200,"context_analysis":"analyzed","body_ref":"b_safe","request_bytes":60000,"context_bytes":100000,"instructions_bytes":10000,"user_bytes":5000,"assistant_bytes":15000,"tool_output_bytes":60000,"other_input_bytes":10000,"context_delta_available":true,"context_growth_bytes":5000,"reused_context_bytes":90000,"novel_context_bytes":10000,"context_reuse_ratio":0.9,"context_amplification_ratio":10,"usage_available":true,"input_tokens":72000,"cached_input_tokens":61000,"output_tokens":1200,"reasoning_tokens":700,"total_tokens":73200,"ignored_raw_field":"SUPER_SECRET_PROMPT_938482"}`,
		`{"ts":"2026-09-10T15:00:06Z","schema_version":1,"event_type":"request_start","request_id":"r_duplicate","method":"POST","route_template":"/backend-api/codex/responses"}`,
		`{"ts":"2026-09-10T15:00:07Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_duplicate","method":"POST","route_template":"/backend-api/codex/responses","attempt":1,"status_code":200}`,
		`{"ts":"2026-09-10T15:00:08Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_duplicate","method":"POST","route_template":"/backend-api/codex/responses","attempt":2,"status_code":200}`,
		`{"ts":"2026-09-10T15:00:09Z","schema_version":1,"event_type":"request_end","request_id":"r_duplicate","method":"POST","route_template":"/backend-api/codex/responses","status_code":200,"context_analysis":"skipped","context_skip_reason":"malformed_json","request_bytes":17}`,
	}, "\n")
	report := NewReport(ReportOptions{Start: base, End: base.Add(time.Hour), Location: time.UTC})
	if err := report.AddReader(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if report.ContextAnalyzed != 1 || report.ContextSkipped != 1 || report.ContextSkipReasons["malformed_json"] != 1 || report.ContextLineageCount != 1 {
		t.Fatalf("context coverage=%+v", report)
	}
	if report.ResponsesRequests != 2 || report.ResponsesUpstreamAttempts != 4 || report.QuotaReplays != 1 || report.SuccessfulGenerations != 3 || report.DuplicateSuccessfulGenerations() != 1 {
		t.Fatalf("inference requests=%d attempts=%d replays=%d successes=%d duplicates=%d", report.ResponsesRequests, report.ResponsesUpstreamAttempts, report.QuotaReplays, report.SuccessfulGenerations, report.DuplicateSuccessfulGenerations())
	}
	var out bytes.Buffer
	report.WriteText(&out)
	text := out.String()
	for _, want := range []string{
		"Context observability:", "responses_post=2 analyzed=1 skipped=1", "malformed_json=1",
		"Request wire size: n=2", "Decoded context size: n=1 p50=97.7KB",
		"Context reuse: n=1 p50=90.0%", "cached_input_tokens=61000", "cache_ratio=84.7%",
		"quota_replays=1", "duplicate_successful_generations=1", "Inference warning: duplicate successful upstream generation detected",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "SUPER_SECRET_PROMPT_938482") {
		t.Fatalf("report leaked ignored raw field:\n%s", text)
	}
}

func TestReportShowsTokenUsageUnavailableWithoutUpstreamUsage(t *testing.T) {
	base := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	input := `{"ts":"2026-09-10T15:00:01Z","schema_version":1,"event_type":"request_end","request_id":"r_no_usage","method":"POST","route_template":"/backend-api/codex/responses","status_code":200,"context_analysis":"analyzed","request_bytes":4096}`
	report := NewReport(ReportOptions{Start: base, End: base.Add(time.Hour), Location: time.UTC})
	if err := report.AddReader(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report.WriteText(&out)
	text := out.String()
	if !strings.Contains(text, "Token usage:\n  unavailable") {
		t.Fatalf("missing explicit unavailable token usage:\n%s", text)
	}
	if !strings.Contains(text, "Decoded context size: n=1 p50=4.0KB") {
		t.Fatalf("legacy analyzed event must fall back to request_bytes for decoded context size:\n%s", text)
	}
	if strings.Contains(strings.ToLower(text), "estimated") {
		t.Fatalf("report must not estimate tokens:\n%s", text)
	}
}

func TestReportTreatsAcceptedWebSocketAsActiveUpgradeNotUnfinished(t *testing.T) {
	base := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	input := strings.Join([]string{
		`{"ts":"2026-09-10T15:00:01Z","schema_version":1,"event_type":"request_start","request_id":"r_ws","method":"GET","route_template":"/backend-api/wham/*","transport":"websocket"}`,
		`{"ts":"2026-09-10T15:00:02Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_ws","method":"GET","route_template":"/backend-api/wham/*","transport":"websocket","status_code":101,"status_origin":"upstream","attempt":1,"upstream_headers_ms":25}`,
	}, "\n")
	report := NewReport(ReportOptions{Start: base, End: base.Add(time.Hour), Location: time.UTC})
	if err := report.AddReader(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if report.ActiveUpgrades() != 1 || report.Unfinished() != 0 {
		t.Fatalf("active_upgrades=%d unfinished=%d", report.ActiveUpgrades(), report.Unfinished())
	}
	var out bytes.Buffer
	report.WriteText(&out)
	text := out.String()
	for _, want := range []string{"active_upgrades=1", "unfinished=0"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Coverage warning") {
		t.Fatalf("active websocket must not mark coverage incomplete:\n%s", text)
	}
}

func TestReportCountsClosedWebSocketAsCompletedNotActive(t *testing.T) {
	base := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	input := strings.Join([]string{
		`{"ts":"2026-09-10T15:00:01Z","schema_version":1,"event_type":"request_start","request_id":"r_ws","transport":"websocket"}`,
		`{"ts":"2026-09-10T15:00:02Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_ws","transport":"websocket","status_code":101,"status_origin":"upstream","attempt":1}`,
		`{"ts":"2026-09-10T15:00:03Z","schema_version":1,"event_type":"request_end","request_id":"r_ws","route_template":"/backend-api/wham/*","transport":"websocket","status_code":101,"status_origin":"upstream","outcome":"upgrade_closed"}`,
	}, "\n")
	report := NewReport(ReportOptions{Start: base, End: base.Add(time.Hour), Location: time.UTC})
	if err := report.AddReader(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if report.ActiveUpgrades() != 0 || report.Unfinished() != 0 || report.Outcomes[OutcomeUpgradeClosed] != 1 {
		t.Fatalf("active=%d unfinished=%d outcomes=%v", report.ActiveUpgrades(), report.Unfinished(), report.Outcomes)
	}
}

func TestReportMarksUpgradeInterruptedByLaterStartup(t *testing.T) {
	base := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	input := strings.Join([]string{
		`{"ts":"2026-09-10T15:00:01Z","schema_version":1,"event_type":"request_start","request_id":"r_ws","transport":"websocket"}`,
		`{"ts":"2026-09-10T15:00:02Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_ws","transport":"websocket","status_code":101,"status_origin":"upstream","attempt":1}`,
		`{"ts":"2026-09-10T15:01:00Z","schema_version":1,"event_type":"startup","service_version":"docker","service_commit":"new"}`,
	}, "\n")
	report := NewReport(ReportOptions{Start: base, End: base.Add(time.Hour), Location: time.UTC})
	if err := report.AddReader(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if report.ActiveUpgrades() != 0 || report.InterruptedUpgrades() != 1 || report.Unfinished() != 0 {
		t.Fatalf("active=%d interrupted=%d unfinished=%d", report.ActiveUpgrades(), report.InterruptedUpgrades(), report.Unfinished())
	}
	var out bytes.Buffer
	report.WriteText(&out)
	text := out.String()
	for _, want := range []string{"active_upgrades=0", "interrupted_upgrades=1", "Coverage warning"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}

func TestReportExcludesWebSocketLifetimeFromGlobalLatency(t *testing.T) {
	base := time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)
	input := strings.Join([]string{
		`{"ts":"2026-09-11T14:00:01Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_http","method":"GET","route_template":"/backend-api/codex/*","transport":"http","status_code":200,"upstream_headers_ms":300}`,
		`{"ts":"2026-09-11T14:00:02Z","schema_version":1,"event_type":"request_end","request_id":"r_http","method":"GET","route_template":"/backend-api/codex/*","transport":"http","status_code":200,"gateway_total_ms":320,"auth_ms":8,"first_body_ms":295}`,
		`{"ts":"2026-09-11T14:00:03Z","schema_version":1,"event_type":"upstream_attempt","request_id":"r_ws","method":"GET","route_template":"/backend-api/wham/*","transport":"websocket","status_code":101,"upstream_headers_ms":1200}`,
		`{"ts":"2026-09-11T14:30:00Z","schema_version":1,"event_type":"request_end","request_id":"r_ws","method":"GET","route_template":"/backend-api/wham/*","transport":"websocket","status_code":101,"gateway_total_ms":8790201.31,"auth_ms":9000,"first_body_ms":8000,"outcome":"upgrade_closed"}`,
	}, "\n")
	report := NewReport(ReportOptions{Start: base, End: base.Add(time.Hour), Location: time.UTC})
	if err := report.AddReader(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if got := report.Latencies["gateway_total_ms"]; len(got) != 1 || got[0] != 320 {
		t.Fatalf("global gateway latency must contain only HTTP request latency, got %v", got)
	}
	if got := report.Latencies["auth_ms"]; len(got) != 1 || got[0] != 8 {
		t.Fatalf("global auth latency must contain only HTTP request latency, got %v", got)
	}
	if got := report.Latencies["first_body_ms"]; len(got) != 1 || got[0] != 295 {
		t.Fatalf("global first-body latency must contain only HTTP request latency, got %v", got)
	}
	if got := report.Latencies["upstream_headers_ms"]; len(got) != 1 || got[0] != 300 {
		t.Fatalf("global upstream-header latency must contain only HTTP attempt latency, got %v", got)
	}
	wsGroup := report.GroupLatencies["/backend-api/wham/* | websocket"]
	if got := wsGroup["gateway_total_ms"]; len(got) != 1 || got[0] != 8790201.31 {
		t.Fatalf("websocket connection lifetime must remain visible in websocket group, got %v", got)
	}

	var out bytes.Buffer
	report.WriteText(&out)
	text := out.String()
	if !strings.Contains(text, "gateway_total_ms: n=1 p50=320.00ms p95=320.00ms p99=320.00ms") {
		t.Fatalf("global latency output must report HTTP-only percentiles:\n%s", text)
	}
	if !strings.Contains(text, "/backend-api/wham/* | websocket") || !strings.Contains(text, "p50=8790201.31ms") {
		t.Fatalf("websocket group latency missing:\n%s", text)
	}
}

func TestReportShowsPromptCacheLineageAndStructuralComposition(t *testing.T) {
	base := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	input := strings.Join([]string{
		`{"ts":"2026-09-12T00:00:01Z","schema_version":1,"event_type":"request_end","request_id":"r1","method":"POST","route_template":"/backend-api/codex/responses","status_code":200,"context_analysis":"analyzed","lineage_source":"prompt_cache_key","request_bytes":100,"context_bytes":1000,"user_bytes":100,"tool_output_bytes":200,"tool_definition_bytes":300,"system_bytes":50,"developer_bytes":50,"reasoning_bytes":100,"metadata_bytes":50,"other_input_bytes":150}`,
		`{"ts":"2026-09-12T00:00:02Z","schema_version":1,"event_type":"request_end","request_id":"r2","method":"POST","route_template":"/backend-api/codex/responses","status_code":200,"context_analysis":"analyzed","lineage_source":"prompt_cache_key","request_bytes":110,"context_bytes":1100,"context_delta_available":true,"context_growth_bytes":100,"context_reuse_ratio":0.8,"context_amplification_ratio":5,"user_bytes":110,"tool_output_bytes":220,"tool_definition_bytes":330,"system_bytes":55,"developer_bytes":55,"reasoning_bytes":110,"metadata_bytes":55,"other_input_bytes":165}`,
	}, "\n")
	report := NewReport(ReportOptions{Start: base, End: base.Add(time.Hour), Location: time.UTC})
	if err := report.AddReader(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if report.ContextLineageKeyCount != 2 || report.ContextLineageCount != 1 || report.ContextLineageSources["prompt_cache_key"] != 2 {
		t.Fatalf("lineage report=%+v", report)
	}
	var out bytes.Buffer
	report.WriteText(&out)
	text := out.String()
	for _, want := range []string{
		"lineage_available=2 lineage_unavailable=0 delta_available=1 delta_unavailable=1",
		"lineage_sources: prompt_cache_key=2",
		"tool_definition=30.0%", "system=5.0%", "developer=5.0%", "reasoning=10.0%", "metadata=5.0%",
		"Context growth: n=1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}
