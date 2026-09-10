package observability

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"
)

type ReportOptions struct {
	Start    time.Time
	End      time.Time
	Location *time.Location
}

type Report struct {
	Start              time.Time
	End                time.Time
	Location           *time.Location
	Lines              int
	Events             int
	Malformed          int
	SchemaMismatch     int
	LatestStartup      time.Time
	Starts             map[string]Event
	Ends               map[string]Event
	UpgradeAccepted    map[string]Event
	EndEvents          []Event
	StatusCounts       map[int]int
	RouteStatuses      map[string]map[int]int
	ErrorCategories    map[string]int
	Outcomes           map[string]int
	SemanticOutcomes   map[string]int
	StatusOrigins      map[string]int
	TransportFallbacks int
	Attempts           int
	DroppedEvents      uint64
	Latencies          map[string][]float64
	GroupLatencies     map[string]map[string][]float64
}

func NewReport(opts ReportOptions) *Report {
	loc := opts.Location
	if loc == nil {
		loc = time.UTC
	}
	return &Report{
		Start: opts.Start, End: opts.End, Location: loc,
		Starts: map[string]Event{}, Ends: map[string]Event{}, UpgradeAccepted: map[string]Event{}, StatusCounts: map[int]int{},
		RouteStatuses: map[string]map[int]int{}, ErrorCategories: map[string]int{}, Outcomes: map[string]int{},
		SemanticOutcomes: map[string]int{}, StatusOrigins: map[string]int{}, Latencies: map[string][]float64{},
		GroupLatencies: map[string]map[string][]float64{},
	}
}

func (r *Report) AddReader(reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		r.Lines++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			r.Malformed++
			continue
		}
		if event.SchemaVersion != SchemaVersion {
			r.SchemaMismatch++
			continue
		}
		if event.Timestamp.IsZero() || event.Timestamp.Before(r.Start) || event.Timestamp.After(r.End) {
			continue
		}
		r.Events++
		switch event.EventType {
		case EventStartup:
			if event.Timestamp.After(r.LatestStartup) {
				r.LatestStartup = event.Timestamp
			}
		case EventRequestStart:
			if event.RequestID != "" {
				r.Starts[event.RequestID] = event
			}
		case EventRequestEnd:
			if event.RequestID != "" {
				r.Ends[event.RequestID] = event
			}
			r.EndEvents = append(r.EndEvents, event)
			r.StatusCounts[event.StatusCode]++
			if r.RouteStatuses[event.RouteTemplate] == nil {
				r.RouteStatuses[event.RouteTemplate] = map[int]int{}
			}
			r.RouteStatuses[event.RouteTemplate][event.StatusCode]++
			if event.ErrorCategory != "" && event.ErrorCategory != "none" {
				r.ErrorCategories[event.ErrorCategory]++
			}
			if event.Outcome != "" {
				r.Outcomes[event.Outcome]++
			}
			if event.SemanticOutcome != "" {
				r.SemanticOutcomes[event.SemanticOutcome]++
			}
			if event.StatusOrigin != "" {
				r.StatusOrigins[event.StatusOrigin]++
			}
			r.addLatency("gateway_total_ms", event.GatewayTotalMS)
			r.addLatency("auth_ms", event.AuthMS)
			r.addLatency("first_body_ms", event.FirstBodyMS)
			group := event.RouteTemplate + " | " + event.Transport
			if r.GroupLatencies[group] == nil {
				r.GroupLatencies[group] = map[string][]float64{}
			}
			if event.GatewayTotalMS > 0 {
				r.GroupLatencies[group]["gateway_total_ms"] = append(r.GroupLatencies[group]["gateway_total_ms"], event.GatewayTotalMS)
			}
			if event.FirstBodyMS > 0 {
				r.GroupLatencies[group]["first_body_ms"] = append(r.GroupLatencies[group]["first_body_ms"], event.FirstBodyMS)
			}
		case EventUpstreamAttempt:
			r.Attempts++
			r.addLatency("upstream_headers_ms", event.UpstreamHeadersMS)
			if event.RequestID != "" && event.StatusCode == 101 && event.Transport == "websocket" {
				r.UpgradeAccepted[event.RequestID] = event
			}
		case EventTransportFallback:
			r.TransportFallbacks++
		case EventLoggerHealth:
			r.DroppedEvents += event.DroppedEvents
		}
	}
	return scanner.Err()
}

func (r *Report) addLatency(name string, value float64) {
	if value > 0 {
		r.Latencies[name] = append(r.Latencies[name], value)
	}
}

func (r *Report) ActiveUpgrades() int {
	count := 0
	for id, upgrade := range r.UpgradeAccepted {
		if _, ended := r.Ends[id]; ended {
			continue
		}
		if r.upgradeInterrupted(upgrade) {
			continue
		}
		count++
	}
	return count
}

func (r *Report) InterruptedUpgrades() int {
	count := 0
	for id, upgrade := range r.UpgradeAccepted {
		if _, ended := r.Ends[id]; ended {
			continue
		}
		if r.upgradeInterrupted(upgrade) {
			count++
		}
	}
	return count
}

func (r *Report) upgradeInterrupted(upgrade Event) bool {
	return !r.LatestStartup.IsZero() && r.LatestStartup.After(upgrade.Timestamp)
}

func (r *Report) Unfinished() int {
	count := 0
	for id := range r.Starts {
		if _, ended := r.Ends[id]; ended {
			continue
		}
		if upgrade, accepted := r.UpgradeAccepted[id]; accepted {
			if !r.upgradeInterrupted(upgrade) {
				continue
			}
			// Interrupted upgrades are reported separately from generic unfinished requests.
			continue
		}
		count++
	}
	return count
}

func (r *Report) EndsWithoutStart() int {
	count := 0
	for id := range r.Ends {
		if _, ok := r.Starts[id]; !ok {
			count++
		}
	}
	return count
}

func (r *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "GPT Codex Router observability report\n")
	fmt.Fprintf(w, "Window: %s -> %s (%s)\n", r.Start.In(r.Location).Format(time.RFC3339), r.End.In(r.Location).Format(time.RFC3339), r.Location)
	fmt.Fprintf(w, "Coverage: lines=%d events=%d starts=%d ends=%d active_upgrades=%d interrupted_upgrades=%d unfinished=%d end_without_start=%d malformed=%d schema_mismatch=%d dropped_events=%d\n",
		r.Lines, r.Events, len(r.Starts), len(r.EndEvents), r.ActiveUpgrades(), r.InterruptedUpgrades(), r.Unfinished(), r.EndsWithoutStart(), r.Malformed, r.SchemaMismatch, r.DroppedEvents)
	if r.DroppedEvents > 0 || r.Malformed > 0 || r.SchemaMismatch > 0 || r.Unfinished() > 0 || r.InterruptedUpgrades() > 0 {
		fmt.Fprintln(w, "Coverage warning: this interval is incomplete; do not use it alone to declare the service healthy or an optimization successful.")
	}
	fmt.Fprintf(w, "Requests: completed=%d active_upgrades=%d interrupted_upgrades=%d upstream_attempts=%d transport_fallbacks=%d\n", len(r.EndEvents), r.ActiveUpgrades(), r.InterruptedUpgrades(), r.Attempts, r.TransportFallbacks)
	writeIntMap(w, "Status", r.StatusCounts)
	writeStringMap(w, "Status origin", r.StatusOrigins)
	writeStringMap(w, "Error category", r.ErrorCategories)
	writeStringMap(w, "Outcome", r.Outcomes)
	writeStringMap(w, "Semantic outcome", r.SemanticOutcomes)

	if len(r.RouteStatuses) > 0 {
		fmt.Fprintln(w, "Route/status:")
		routes := sortedKeys(r.RouteStatuses)
		for _, route := range routes {
			statuses := r.RouteStatuses[route]
			keys := make([]int, 0, len(statuses))
			for code := range statuses {
				keys = append(keys, code)
			}
			sort.Ints(keys)
			parts := make([]string, 0, len(keys))
			for _, code := range keys {
				parts = append(parts, fmt.Sprintf("%d=%d", code, statuses[code]))
			}
			fmt.Fprintf(w, "  %s: %s\n", route, strings.Join(parts, " "))
		}
	}

	fmt.Fprintln(w, "Latency:")
	for _, name := range []string{"gateway_total_ms", "auth_ms", "upstream_headers_ms", "first_body_ms"} {
		writePercentiles(w, "  "+name, r.Latencies[name])
	}
	if len(r.GroupLatencies) > 0 {
		fmt.Fprintln(w, "Latency by route/transport:")
		groups := sortedKeys(r.GroupLatencies)
		for _, group := range groups {
			metrics := r.GroupLatencies[group]
			fmt.Fprintf(w, "  %s\n", group)
			writePercentiles(w, "    gateway_total_ms", metrics["gateway_total_ms"])
			writePercentiles(w, "    first_body_ms", metrics["first_body_ms"])
		}
	}
	if len(r.EndEvents) < 30 {
		fmt.Fprintf(w, "Sample warning: only %d completed requests; p95/p99 are unstable and should not drive optimization decisions alone.\n", len(r.EndEvents))
	}
}

func writeIntMap(w io.Writer, label string, values map[int]int) {
	if len(values) == 0 {
		fmt.Fprintf(w, "%s: none\n", label)
		return
	}
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%d=%d", key, values[key]))
	}
	fmt.Fprintf(w, "%s: %s\n", label, strings.Join(parts, " "))
}

func writeStringMap(w io.Writer, label string, values map[string]int) {
	if len(values) == 0 {
		fmt.Fprintf(w, "%s: none\n", label)
		return
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	fmt.Fprintf(w, "%s: %s\n", label, strings.Join(parts, " "))
}

func writePercentiles(w io.Writer, label string, values []float64) {
	if len(values) == 0 {
		fmt.Fprintf(w, "%s: n=0\n", label)
		return
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	fmt.Fprintf(w, "%s: n=%d p50=%.2fms p95=%.2fms p99=%.2fms\n", label, len(copyValues), percentile(copyValues, 0.50), percentile(copyValues, 0.95), percentile(copyValues, 0.99))
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if len(values) == 1 {
		return values[0]
	}
	pos := p * float64(len(values)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return values[lo]
	}
	fraction := pos - float64(lo)
	return values[lo] + (values[hi]-values[lo])*fraction
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
