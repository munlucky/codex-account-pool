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
