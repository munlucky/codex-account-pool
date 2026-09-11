package observability

import "time"

const SchemaVersion = 1

const (
	EventStartup           = "startup"
	EventRequestStart      = "request_start"
	EventUpstreamAttempt   = "upstream_attempt"
	EventAccountSwitch     = "account_switch"
	EventTransportFallback = "transport_fallback"
	EventRequestEnd        = "request_end"
	EventLoggerHealth      = "logger_health"
)

const (
	OutcomeBodyEOF              = "body_eof"
	OutcomeClientCancel         = "client_cancel"
	OutcomeUpstreamReadError    = "upstream_read_error"
	OutcomeDownstreamWriteError = "downstream_write_error"
	OutcomeUpgradeClosed        = "upgrade_closed"
	OutcomeLocalResponse        = "local_response"
	OutcomeUnknown              = "unknown"
)

const (
	SemanticCompleted     = "completed"
	SemanticFailed        = "failed"
	SemanticIncomplete    = "incomplete"
	SemanticUnobserved    = "unobserved"
	SemanticNotApplicable = "not_applicable"
)

type Event struct {
	Timestamp         time.Time `json:"ts"`
	SchemaVersion     int       `json:"schema_version"`
	EventType         string    `json:"event_type"`
	ServiceVersion    string    `json:"service_version,omitempty"`
	ServiceCommit     string    `json:"service_commit,omitempty"`
	RequestID         string    `json:"request_id,omitempty"`
	Method            string    `json:"method,omitempty"`
	RouteTemplate     string    `json:"route_template,omitempty"`
	Transport         string    `json:"transport,omitempty"`
	PeerClass         string    `json:"peer_class,omitempty"`
	ProfileRef        string    `json:"profile_ref,omitempty"`
	StatusCode        int       `json:"status_code,omitempty"`
	StatusOrigin      string    `json:"status_origin,omitempty"`
	Outcome           string    `json:"outcome,omitempty"`
	ErrorCategory     string    `json:"error_category,omitempty"`
	Attempt           int       `json:"attempt,omitempty"`
	GatewayTotalMS    float64   `json:"gateway_total_ms,omitempty"`
	AuthMS            float64   `json:"auth_ms,omitempty"`
	UpstreamHeadersMS float64   `json:"upstream_headers_ms,omitempty"`
	FirstBodyMS       float64   `json:"first_body_ms,omitempty"`
	ResponseBytes     int64     `json:"response_bytes,omitempty"`
	SemanticOutcome   string    `json:"semantic_outcome,omitempty"`
	SwitchFromRef     string    `json:"switch_from_ref,omitempty"`
	SwitchToRef       string    `json:"switch_to_ref,omitempty"`
	TransportFallback bool      `json:"transport_fallback,omitempty"`
	DroppedEvents     uint64    `json:"dropped_events,omitempty"`

	ContextAnalysis           string  `json:"context_analysis,omitempty"`
	ContextSkipReason         string  `json:"context_skip_reason,omitempty"`
	LineageSource             string  `json:"lineage_source,omitempty"`
	BodyRef                   string  `json:"body_ref,omitempty"`
	RequestBytes              int64   `json:"request_bytes,omitempty"`
	ContextBytes              int64   `json:"context_bytes,omitempty"`
	InstructionsBytes         int64   `json:"instructions_bytes,omitempty"`
	UserBytes                 int64   `json:"user_bytes,omitempty"`
	AssistantBytes            int64   `json:"assistant_bytes,omitempty"`
	ToolOutputBytes           int64   `json:"tool_output_bytes,omitempty"`
	ToolDefinitionBytes       int64   `json:"tool_definition_bytes,omitempty"`
	SystemBytes               int64   `json:"system_bytes,omitempty"`
	DeveloperBytes            int64   `json:"developer_bytes,omitempty"`
	ReasoningBytes            int64   `json:"reasoning_bytes,omitempty"`
	MetadataBytes             int64   `json:"metadata_bytes,omitempty"`
	OtherInputBytes           int64   `json:"other_input_bytes,omitempty"`
	InputItems                int     `json:"input_items,omitempty"`
	UserItems                 int     `json:"user_items,omitempty"`
	AssistantItems            int     `json:"assistant_items,omitempty"`
	ToolItems                 int     `json:"tool_items,omitempty"`
	ContextDeltaAvailable     bool    `json:"context_delta_available,omitempty"`
	PreviousRequestBytes      int64   `json:"previous_request_bytes,omitempty"`
	PreviousContextBytes      int64   `json:"previous_context_bytes,omitempty"`
	ContextGrowthBytes        int64   `json:"context_growth_bytes,omitempty"`
	ReusedItemCount           int     `json:"reused_item_count,omitempty"`
	ReusedContextBytes        int64   `json:"reused_context_bytes,omitempty"`
	NovelContextBytes         int64   `json:"novel_context_bytes,omitempty"`
	ContextReuseRatio         float64 `json:"context_reuse_ratio,omitempty"`
	NovelContextRatio         float64 `json:"novel_context_ratio,omitempty"`
	ContextAmplificationRatio float64 `json:"context_amplification_ratio,omitempty"`
	SSEEventCount             int     `json:"sse_event_count,omitempty"`
	OutputItemCount           int     `json:"output_item_count,omitempty"`
	ToolCallCount             int     `json:"tool_call_count,omitempty"`
	UsageAvailable            bool    `json:"usage_available,omitempty"`
	InputTokens               int64   `json:"input_tokens,omitempty"`
	CachedInputTokens         int64   `json:"cached_input_tokens,omitempty"`
	OutputTokens              int64   `json:"output_tokens,omitempty"`
	ReasoningTokens           int64   `json:"reasoning_tokens,omitempty"`
	TotalTokens               int64   `json:"total_tokens,omitempty"`
}
