// Package model defines the wlog domain types, the single-writer Batch unit,
// and the storage interfaces consumed by the ingest and otel packages.
// It depends only on the standard library so it sits at the base of the
// dependency graph (no import cycles).
package model

import "context"

// Tokens holds the four token counters tracked per API request / metric point.
// All values are absolute integer token counts.
type Tokens struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
}

// Session is a single Claude Code (or OTLP-compatible) session. FirstSeen and
// LastSeen are unix-ms event times (falling back to receive time when absent).
// TokenSource is "events" or "metrics" (display hint, never used to split
// metrics). AttrsJSON preserves the original resource/scope attributes.
//
// Repo and GitBranch are the cross-developer/repo aggregation dimensions
// (STRATEGY v3 paid-line data source, PLAN v3 feature 3). OTLP does not carry
// git/repo attributes (PLAN v3 §9 OQ4 resolution), so these are populated from
// the JSONL transcript: Repo is derived from the session cwd (the project
// directory name) and GitBranch from the transcript's gitBranch field. They are
// empty when unknown (non-git dir / OTLP-only session) and an upsert must not
// overwrite a known value with an empty one.
type Session struct {
	ID          string
	FirstSeen   int64
	LastSeen    int64
	AppVersion  string
	ModelSet    string
	UserEmail   string
	OrgID       string
	TokenSource string
	HasEvents   bool
	Repo        string // project/repo name derived from cwd (aggregation dimension)
	GitBranch   string // git branch from the transcript gitBranch field (may be empty)
	AttrsJSON   string
}

// APIRequest is a normalized api_request / api_error event. CostUSD is in USD,
// DurationMS in milliseconds, TS is unix-ms. DedupKey enforces idempotency.
//
// CostSource records the provenance/authority of CostUSD + Tokens so the store
// can resolve a dedup_key collision between the two ingestion paths correctly
// (PLAN v3 §9, the cost-accuracy gate). The OTLP api_request event carries the
// real cost_usd Claude Code computed, so it is authoritative (CostSourceOTLP);
// the JSONL transcript carries no cost, so the parser derives an estimate from
// the pricing table (CostSourceJSONLEstimate) which is $0 for an unknown model.
// When both paths hash to the SAME dedup_key, the OTLP (authoritative) cost must
// win and a JSONL re-scan must NOT overwrite an OTLP-filled row with an estimate
// — see store.insertAPIRequests' ON CONFLICT DO UPDATE guard.
type APIRequest struct {
	ID         int64
	SessionID  string
	TS         int64
	Model      string
	Tokens     Tokens
	CostUSD    float64
	DurationMS int64
	IsError    bool
	StatusCode int
	CostSource string // CostSourceOTLP (authoritative) | CostSourceJSONLEstimate
	DedupKey   string
	AttrsJSON  string
}

// Cost-source provenance values for APIRequest.CostSource (PLAN v3 §9). These
// strings are persisted in api_requests.cost_source and compared in the store's
// upsert, so they are part of the on-disk contract — do not change the literals.
const (
	// CostSourceOTLP marks a cost/token figure that came from the OTLP
	// api_request event (Claude Code's own cost_usd). It is authoritative.
	CostSourceOTLP = "otlp"
	// CostSourceJSONLEstimate marks a cost figure derived by wlog from the
	// pricing table because the JSONL transcript carries no cost ($0 when the
	// model is unknown). It must never overwrite an OTLP figure.
	CostSourceJSONLEstimate = "jsonl-estimate"
)

// ToolDecision is a normalized tool_decision event (accept/reject and source).
type ToolDecision struct {
	ID        int64
	SessionID string
	TS        int64
	Decision  string
	Source    string
	ToolName  string
	DedupKey  string
	AttrsJSON string
}

// ToolResult is a normalized tool_result event. BashCommand and MCPServer are
// extracted from the nested tool_parameters JSON when present.
type ToolResult struct {
	ID          int64
	SessionID   string
	TS          int64
	ToolName    string
	Success     bool
	DurationMS  int64
	BashCommand string
	MCPServer   string
	DedupKey    string
	AttrsJSON   string
}

// Event is a generic (including unclassified) normalized log event. Name is the
// prefix-normalized event name. DedupKey enforces idempotency.
type Event struct {
	ID        int64
	SessionID string
	TS        int64
	Name      string
	DedupKey  string
	AttrsJSON string
}

// Span is a raw-stored OTLP trace span (M1 stores raw; visual linking is M2).
// StartTS / EndTS are unix-ms. Uniqueness is (TraceID, SpanID).
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	SessionID    string
	Name         string
	StartTS      int64
	EndTS        int64
	Status       string
	AttrsJSON    string
}

// MetricPoint is one delta-normalized metric data point. ValueDelta is the
// per-point delta after temporality normalization. ValueKind distinguishes
// aggregation semantics (e.g. sum vs last). SeriesKey + StartUnixNano +
// TimeUnixNano are unique (idempotency for DELTA points).
type MetricPoint struct {
	ID            int64
	SessionID     string
	TS            int64
	Name          string
	AttrKey       string
	ValueDelta    float64
	ValueKind     int
	SeriesKey     string
	StartUnixNano int64
	TimeUnixNano  int64
	AttrsJSON     string
}

// SeriesState is the persisted per-series cursor used by delta normalization
// (§7). BaselineKnown gates the first-observation baseline behaviour.
type SeriesState struct {
	Key               string
	LastValue         float64
	LastStartUnixNano int64
	LastPointUnixNano int64
	Temporality       int32
	BaselineKnown     bool
	Updated           int64
}

// Batch is the unit a single writer applies in one transaction. All slices are
// applied atomically together with the series_state updates.
type Batch struct {
	Sessions      []Session
	APIRequests   []APIRequest
	ToolDecisions []ToolDecision
	ToolResults   []ToolResult
	Events        []Event
	Spans         []Span
	MetricPoints  []MetricPoint
	SeriesStates  []SeriesState
}

// Writer applies a Batch atomically. Implementations serialize all writes
// through a single writer goroutine/connection (store package).
type Writer interface {
	WriteBatch(ctx context.Context, b Batch) error
}

// SeriesStateStore loads the persisted series cursors at startup so the otel
// delta normalizer can resume without re-baselining. This interface lets otel
// depend on model rather than store (dependency inversion).
type SeriesStateStore interface {
	LoadSeriesStates(ctx context.Context) ([]SeriesState, error)
}
