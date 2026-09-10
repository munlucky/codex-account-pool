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
}
