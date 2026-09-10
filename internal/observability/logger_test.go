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
