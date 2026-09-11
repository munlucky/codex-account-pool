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

	ContextAnalyzed        int
	ContextSkipped         int
	ContextSkipReasons     map[string]int
	ContextRequestBytes    []float64
	ContextDecodedBytes    []float64
	ContextGrowthBytes     []float64
	ContextReuseRatios     []float64
	ContextAmplifications  []float64
	ContextLineageCount    int
	ContextLineageKeyCount int
	ContextLineageSources  map[string]int
	InstructionsBytes      int64
	UserBytes              int64
	AssistantBytes         int64
	ToolOutputBytes        int64
	ToolDefinitionBytes    int64
	SystemBytes            int64
	DeveloperBytes         int64
	ReasoningBytes         int64
	MetadataBytes          int64
	OtherInputBytes        int64
	UsageRequests          int
	InputTokens            int64
	CachedInputTokens      int64
	OutputTokens           int64
	ReasoningTokens        int64
	TotalTokens            int64

	ResponsesRequests           int
	ResponsesUpstreamAttempts   int
	QuotaReplays                int
	SuccessfulGenerations       int
	SuccessfulAttemptsByRequest map[string]int
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
		GroupLatencies: map[string]map[string][]float64{}, ContextSkipReasons: map[string]int{},
		ContextLineageSources: map[string]int{}, SuccessfulAttemptsByRequest: map[string]int{},
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
			if event.RouteTemplate == "/backend-api/codex/responses" && event.Method == "POST" {
				r.ResponsesRequests++
			}
			r.addContextEvent(event)
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
			// Global latency percentiles describe bounded HTTP request/response work.
			// A WebSocket request_end records the lifetime of the upgraded connection,
			// which can be hours and must remain visible only in its transport group.
			if event.Transport == "http" {
				r.addLatency("gateway_total_ms", event.GatewayTotalMS)
				r.addLatency("auth_ms", event.AuthMS)
				r.addLatency("first_body_ms", event.FirstBodyMS)
			}
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
			if event.Transport == "http" {
				r.addLatency("upstream_headers_ms", event.UpstreamHeadersMS)
			}
			if event.RouteTemplate == "/backend-api/codex/responses" && event.Method == "POST" {
				r.ResponsesUpstreamAttempts++
				if event.StatusCode >= 200 && event.StatusCode < 300 {
					r.SuccessfulGenerations++
					if event.RequestID != "" {
						r.SuccessfulAttemptsByRequest[event.RequestID]++
					}
				}
			}
			if event.RequestID != "" && event.StatusCode == 101 && event.Transport == "websocket" {
				r.UpgradeAccepted[event.RequestID] = event
			}
		case EventAccountSwitch:
			if event.RouteTemplate == "/backend-api/codex/responses" && event.Method == "POST" {
				r.QuotaReplays++
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

func (r *Report) addContextEvent(event Event) {
	if event.ContextAnalysis == "" {
		return
	}
	switch event.ContextAnalysis {
	case "analyzed":
		r.ContextAnalyzed++
	case "skipped":
		r.ContextSkipped++
		if event.ContextSkipReason != "" {
			r.ContextSkipReasons[event.ContextSkipReason]++
		}
	}
	r.ContextRequestBytes = append(r.ContextRequestBytes, float64(event.RequestBytes))
	if event.LineageSource != "" {
		r.ContextLineageKeyCount++
		r.ContextLineageSources[event.LineageSource]++
	}
	decodedBytes := event.ContextBytes
	if decodedBytes == 0 && event.ContextAnalysis == "analyzed" {
		// Legacy analyzed events predate context_bytes and were uncompressed.
		decodedBytes = event.RequestBytes
	}
	if decodedBytes > 0 {
		r.ContextDecodedBytes = append(r.ContextDecodedBytes, float64(decodedBytes))
	}
	r.InstructionsBytes += event.InstructionsBytes
	r.UserBytes += event.UserBytes
	r.AssistantBytes += event.AssistantBytes
	r.ToolOutputBytes += event.ToolOutputBytes
	r.ToolDefinitionBytes += event.ToolDefinitionBytes
	r.SystemBytes += event.SystemBytes
	r.DeveloperBytes += event.DeveloperBytes
	r.ReasoningBytes += event.ReasoningBytes
	r.MetadataBytes += event.MetadataBytes
	r.OtherInputBytes += event.OtherInputBytes
	if event.ContextDeltaAvailable {
		r.ContextLineageCount++
		r.ContextGrowthBytes = append(r.ContextGrowthBytes, float64(event.ContextGrowthBytes))
		r.ContextReuseRatios = append(r.ContextReuseRatios, event.ContextReuseRatio)
		r.ContextAmplifications = append(r.ContextAmplifications, event.ContextAmplificationRatio)
	}
	if event.UsageAvailable {
		r.UsageRequests++
		r.InputTokens += event.InputTokens
		r.CachedInputTokens += event.CachedInputTokens
		r.OutputTokens += event.OutputTokens
		r.ReasoningTokens += event.ReasoningTokens
		r.TotalTokens += event.TotalTokens
	}
}

func (r *Report) DuplicateSuccessfulGenerations() int {
	count := 0
	for _, successes := range r.SuccessfulAttemptsByRequest {
		if successes > 1 {
			count += successes - 1
		}
	}
	return count
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
	r.writeContext(w)

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

func (r *Report) writeContext(w io.Writer) {
	fmt.Fprintln(w, "Context observability:")
	lineageUnavailable := r.ContextAnalyzed - r.ContextLineageKeyCount
	if lineageUnavailable < 0 {
		lineageUnavailable = 0
	}
	deltaUnavailable := r.ContextLineageKeyCount - r.ContextLineageCount
	if deltaUnavailable < 0 {
		deltaUnavailable = 0
	}
	unobserved := r.ResponsesRequests - r.ContextAnalyzed - r.ContextSkipped
	if unobserved < 0 {
		unobserved = 0
	}
	fmt.Fprintf(w, "  coverage: responses_post=%d analyzed=%d skipped=%d unobserved=%d lineage_available=%d lineage_unavailable=%d delta_available=%d delta_unavailable=%d\n",
		r.ResponsesRequests, r.ContextAnalyzed, r.ContextSkipped, unobserved, r.ContextLineageKeyCount, lineageUnavailable, r.ContextLineageCount, deltaUnavailable)
	if len(r.ContextLineageSources) > 0 {
		keys := sortedKeys(r.ContextLineageSources)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", key, r.ContextLineageSources[key]))
		}
		fmt.Fprintf(w, "  lineage_sources: %s\n", strings.Join(parts, " "))
	}
	if unobserved > 0 {
		fmt.Fprintln(w, "Context coverage warning: some Responses POST requests have no context telemetry; do not generalize context ratios from this interval alone.")
	}
	if len(r.ContextSkipReasons) > 0 {
		keys := sortedKeys(r.ContextSkipReasons)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", key, r.ContextSkipReasons[key]))
		}
		fmt.Fprintf(w, "  skipped: %s\n", strings.Join(parts, " "))
	}
	writeBytePercentiles(w, "Request wire size", r.ContextRequestBytes)
	writeBytePercentiles(w, "Decoded context size", r.ContextDecodedBytes)
	writeBytePercentiles(w, "Context growth", r.ContextGrowthBytes)
	writeRatioPercentiles(w, "Context reuse", r.ContextReuseRatios)
	writeAmplificationPercentiles(w, "Context amplification", r.ContextAmplifications)

	compositionTotal := r.InstructionsBytes + r.UserBytes + r.AssistantBytes + r.ToolOutputBytes +
		r.ToolDefinitionBytes + r.SystemBytes + r.DeveloperBytes + r.ReasoningBytes + r.MetadataBytes + r.OtherInputBytes
	fmt.Fprintln(w, "Context composition:")
	if compositionTotal <= 0 {
		fmt.Fprintln(w, "  unavailable")
	} else {
		fmt.Fprintf(w, "  instructions=%.1f%% user=%.1f%% assistant=%.1f%% tool_output=%.1f%% tool_definition=%.1f%% system=%.1f%% developer=%.1f%% reasoning=%.1f%% metadata=%.1f%% other=%.1f%%\n",
			percentOf(r.InstructionsBytes, compositionTotal), percentOf(r.UserBytes, compositionTotal),
			percentOf(r.AssistantBytes, compositionTotal), percentOf(r.ToolOutputBytes, compositionTotal),
			percentOf(r.ToolDefinitionBytes, compositionTotal), percentOf(r.SystemBytes, compositionTotal),
			percentOf(r.DeveloperBytes, compositionTotal), percentOf(r.ReasoningBytes, compositionTotal),
			percentOf(r.MetadataBytes, compositionTotal), percentOf(r.OtherInputBytes, compositionTotal))
	}

	fmt.Fprintln(w, "Token usage:")
	if r.UsageRequests == 0 {
		fmt.Fprintln(w, "  unavailable")
	} else {
		cacheRatio := 0.0
		if r.InputTokens > 0 {
			cacheRatio = float64(r.CachedInputTokens) / float64(r.InputTokens) * 100
		}
		fmt.Fprintf(w, "  usage_requests=%d input_tokens=%d cached_input_tokens=%d cache_ratio=%.1f%% output_tokens=%d reasoning_tokens=%d total_tokens=%d\n",
			r.UsageRequests, r.InputTokens, r.CachedInputTokens, cacheRatio, r.OutputTokens, r.ReasoningTokens, r.TotalTokens)
	}

	fmt.Fprintln(w, "Inference:")
	fmt.Fprintf(w, "  responses_requests=%d upstream_attempts=%d quota_replays=%d successful_generations=%d duplicate_successful_generations=%d\n",
		r.ResponsesRequests, r.ResponsesUpstreamAttempts, r.QuotaReplays, r.SuccessfulGenerations, r.DuplicateSuccessfulGenerations())

	toolRelatedBytes := r.ToolOutputBytes + r.ToolDefinitionBytes
	if compositionTotal > 0 && percentOf(toolRelatedBytes, compositionTotal) >= 50 {
		fmt.Fprintf(w, "Context warning: high tool-related share: %.1f%% (output %.1f%% + definitions %.1f%%)\n",
			percentOf(toolRelatedBytes, compositionTotal), percentOf(r.ToolOutputBytes, compositionTotal), percentOf(r.ToolDefinitionBytes, compositionTotal))
	}
	if len(r.ContextAmplifications) > 0 {
		values := sortedFloatCopy(r.ContextAmplifications)
		p95 := percentile(values, 0.95)
		if p95 >= 20 {
			fmt.Fprintf(w, "Context warning: request amplification p95: %.1fx\n", p95)
		}
	}
	if duplicates := r.DuplicateSuccessfulGenerations(); duplicates > 0 {
		fmt.Fprintf(w, "Inference warning: duplicate successful upstream generation detected: %d\n", duplicates)
	}
}

func percentOf(value, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(value) / float64(total) * 100
}

func writeBytePercentiles(w io.Writer, label string, values []float64) {
	if len(values) == 0 {
		fmt.Fprintf(w, "%s: n=0\n", label)
		return
	}
	sorted := sortedFloatCopy(values)
	fmt.Fprintf(w, "%s: n=%d p50=%s p95=%s p99=%s\n", label, len(sorted),
		formatBytes(percentile(sorted, 0.50)), formatBytes(percentile(sorted, 0.95)), formatBytes(percentile(sorted, 0.99)))
}

func writeRatioPercentiles(w io.Writer, label string, values []float64) {
	if len(values) == 0 {
		fmt.Fprintf(w, "%s: n=0\n", label)
		return
	}
	sorted := sortedFloatCopy(values)
	fmt.Fprintf(w, "%s: n=%d p50=%.1f%% p95=%.1f%% p99=%.1f%%\n", label, len(sorted),
		percentile(sorted, 0.50)*100, percentile(sorted, 0.95)*100, percentile(sorted, 0.99)*100)
}

func writeAmplificationPercentiles(w io.Writer, label string, values []float64) {
	if len(values) == 0 {
		fmt.Fprintf(w, "%s: n=0\n", label)
		return
	}
	sorted := sortedFloatCopy(values)
	fmt.Fprintf(w, "%s: n=%d p50=%.1fx p95=%.1fx p99=%.1fx\n", label, len(sorted),
		percentile(sorted, 0.50), percentile(sorted, 0.95), percentile(sorted, 0.99))
}

func sortedFloatCopy(values []float64) []float64 {
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	return copyValues
}

func formatBytes(value float64) string {
	abs := math.Abs(value)
	switch {
	case abs >= 1024*1024:
		return fmt.Sprintf("%.1fMB", value/(1024*1024))
	case abs >= 1024:
		return fmt.Sprintf("%.1fKB", value/1024)
	default:
		return fmt.Sprintf("%.0fB", value)
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
