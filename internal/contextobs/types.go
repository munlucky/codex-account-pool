package contextobs

import "time"

const (
	AnalysisAnalyzed = "analyzed"
	AnalysisSkipped  = "skipped"

	SkipNone                = ""
	SkipMalformed           = "malformed_json"
	SkipOversized           = "oversized_payload"
	SkipUnsupported         = "unsupported_payload"
	SkipUnsupportedEncoding = "unsupported_encoding"
	SkipDecodeError         = "decode_error"
)

const DefaultMaxAnalyzedBodyBytes = 8 << 20 // 8 MiB

// ItemRef is process-local structural metadata. It never contains source content.
type ItemRef struct {
	Type string
	Size int64
	Ref  string
}

// RequestMetrics contains metadata derived from the exact replayable request bytes.
type RequestMetrics struct {
	// RequestBytes is the exact encoded wire-body size forwarded upstream.
	RequestBytes int64 `json:"request_bytes,omitempty"`
	// ContextBytes is the decoded JSON representation used only for analysis.
	ContextBytes int64  `json:"context_bytes,omitempty"`
	BodyRef      string `json:"body_ref,omitempty"`

	AnalysisStatus string `json:"analysis_status,omitempty"`
	SkipReason     string `json:"skip_reason,omitempty"`

	InstructionsBytes   int64 `json:"instructions_bytes,omitempty"`
	UserBytes           int64 `json:"user_bytes,omitempty"`
	AssistantBytes      int64 `json:"assistant_bytes,omitempty"`
	ToolOutputBytes     int64 `json:"tool_output_bytes,omitempty"`
	ToolDefinitionBytes int64 `json:"tool_definition_bytes,omitempty"`
	SystemBytes         int64 `json:"system_bytes,omitempty"`
	DeveloperBytes      int64 `json:"developer_bytes,omitempty"`
	ReasoningBytes      int64 `json:"reasoning_bytes,omitempty"`
	MetadataBytes       int64 `json:"metadata_bytes,omitempty"`
	OtherInputBytes     int64 `json:"other_input_bytes,omitempty"`

	InputItems     int `json:"input_items,omitempty"`
	UserItems      int `json:"user_items,omitempty"`
	AssistantItems int `json:"assistant_items,omitempty"`
	ToolItems      int `json:"tool_items,omitempty"`

	ItemRefs            []ItemRef `json:"-"`
	LineageRef          string    `json:"-"`
	LineageSource       string    `json:"lineage_source,omitempty"`
	PreviousResponseRef string    `json:"-"`
}

// DeltaMetrics describes structural reuse against an observed predecessor.
type DeltaMetrics struct {
	Available bool

	// PreviousRequestBytes is retained as a compatibility alias for the decoded
	// predecessor context size. PreviousContextBytes is the explicit name.
	PreviousRequestBytes int64
	PreviousContextBytes int64
	ContextGrowthBytes   int64
	ReusedItemCount      int
	ReusedContextBytes   int64
	NovelContextBytes    int64

	ContextReuseRatio         float64
	NovelContextRatio         float64
	ContextAmplificationRatio float64
}

// ResponseMetrics contains numeric/bounded metadata extracted incrementally from SSE.
type ResponseMetrics struct {
	SSEEventCount   int
	OutputItemCount int
	ToolCallCount   int

	UsageAvailable    bool
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
	ReasoningTokens   int64
	TotalTokens       int64

	ResponseRef string `json:"-"`
}

// TrackerOptions bounds process-local lineage state.
type TrackerOptions struct {
	MaxEntries int
	TTL        time.Duration
}
