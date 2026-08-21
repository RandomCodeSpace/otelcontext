// Package graphrag provides a layered in-memory graph for real-time
// observability retrieval — error chains, root cause analysis, impact analysis.
// It replaces the simpler internal/graph package with typed stores.
package graphrag

import (
	"time"
)

// --- Node Types ---

// NodeType distinguishes different node categories in the graph.
type NodeType string

const (
	NodeService    NodeType = "service"
	NodeOperation  NodeType = "operation"
	NodeTrace      NodeType = "trace"
	NodeSpan       NodeType = "span"
	NodeLogCluster NodeType = "log_cluster"
	NodeMetric     NodeType = "metric"
	NodeAnomaly    NodeType = "anomaly"
)

// ServiceNode represents a microservice with aggregated health stats.
type ServiceNode struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	HealthScore float64   `json:"health_score"` // 0.0–1.0

	CallCount  int64   `json:"call_count"`
	ErrorCount int64   `json:"error_count"`
	ErrorRate  float64 `json:"error_rate"`
	AvgLatency float64 `json:"avg_latency_ms"`
	TotalMs    float64 `json:"-"` // for computing avg
}

// OperationNode represents an endpoint/RPC within a service.
type OperationNode struct {
	ID          string    `json:"id"` // service + "|" + operation
	Service     string    `json:"service"`
	Operation   string    `json:"operation"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	HealthScore float64   `json:"health_score"`

	CallCount  int64   `json:"call_count"`
	ErrorCount int64   `json:"error_count"`
	ErrorRate  float64 `json:"error_rate"`
	AvgLatency float64 `json:"avg_latency_ms"`
	P50Latency float64 `json:"p50_latency_ms"`
	P95Latency float64 `json:"p95_latency_ms"`
	P99Latency float64 `json:"p99_latency_ms"`
	TotalMs    float64 `json:"-"`
}

// TraceNode represents a distributed trace.
type TraceNode struct {
	ID          string    `json:"id"` // trace_id
	RootService string    `json:"root_service"`
	Duration    float64   `json:"duration_ms"`
	Status      string    `json:"status"`
	Timestamp   time.Time `json:"timestamp"`
	SpanCount   int       `json:"span_count"`
}

// SpanNode represents a single span within a trace.
type SpanNode struct {
	ID           string    `json:"id"` // span_id
	TraceID      string    `json:"trace_id"`
	ParentSpanID string    `json:"parent_span_id"`
	Service      string    `json:"service"`
	Operation    string    `json:"operation"`
	Duration     float64   `json:"duration_ms"`
	StatusCode   string    `json:"status_code"`
	IsError      bool      `json:"is_error"`
	Timestamp    time.Time `json:"timestamp"`
}

// LogClusterNode groups similar log messages.
//
// Clustering is performed by the Drain template miner. TemplateID is the
// stable FNV-64 hash of TemplateTokens; ID is the user-facing cluster
// identifier (service-scoped) that remains stable across Drain re-merges.
type LogClusterNode struct {
	ID             string           `json:"id"` // service-scoped cluster id (stable)
	Template       string           `json:"template"`
	TemplateID     uint64           `json:"template_id,omitempty"`
	TemplateTokens []string         `json:"template_tokens,omitempty"`
	SampleLog      string           `json:"sample_log,omitempty"`
	Count          int64            `json:"count"`
	FirstSeen      time.Time        `json:"first_seen"`
	LastSeen       time.Time        `json:"last_seen"`
	SeverityDist   map[string]int64 `json:"severity_distribution"`
}

// MetricNode represents a metric series for a service.
type MetricNode struct {
	ID          string    `json:"id"` // metric_name + "|" + service
	MetricName  string    `json:"metric_name"`
	Service     string    `json:"service"`
	RollingMin  float64   `json:"rolling_min"`
	RollingMax  float64   `json:"rolling_max"`
	RollingAvg  float64   `json:"rolling_avg"`
	SampleCount int64     `json:"sample_count"`
	LastSeen    time.Time `json:"last_seen"`
}

// AnomalySeverity indicates the severity of an anomaly.
type AnomalySeverity string

const (
	SeverityCritical AnomalySeverity = "critical"
	SeverityWarning  AnomalySeverity = "warning"
	SeverityInfo     AnomalySeverity = "info"
)

// AnomalyType indicates the kind of anomaly detected.
type AnomalyType string

const (
	AnomalyErrorSpike   AnomalyType = "error_spike"
	AnomalyLatencySpike AnomalyType = "latency_spike"
	AnomalyMetricZScore AnomalyType = "metric_zscore"
)

// AnomalyNode represents a detected anomaly.
type AnomalyNode struct {
	ID        string          `json:"id"`
	Type      AnomalyType     `json:"type"`
	Severity  AnomalySeverity `json:"severity"`
	Service   string          `json:"service"`
	Evidence  string          `json:"evidence"`
	Timestamp time.Time       `json:"timestamp"`
}

// --- Edge Types ---

// EdgeType distinguishes different relationship categories.
type EdgeType string

const (
	EdgeCalls        EdgeType = "CALLS"
	EdgeExposes      EdgeType = "EXPOSES"
	EdgeContains     EdgeType = "CONTAINS"
	EdgeChildOf      EdgeType = "CHILD_OF"
	EdgeEmittedBy    EdgeType = "EMITTED_BY"
	EdgeLoggedDuring EdgeType = "LOGGED_DURING"
	EdgeMeasuredBy   EdgeType = "MEASURED_BY"
	EdgePrecededBy   EdgeType = "PRECEDED_BY"
	EdgeTriggeredBy  EdgeType = "TRIGGERED_BY"
)

// Edge represents a directed relationship between two nodes.
type Edge struct {
	Type       EdgeType  `json:"type"`
	FromID     string    `json:"from_id"`
	ToID       string    `json:"to_id"`
	Weight     float64   `json:"weight,omitempty"`
	CallCount  int64     `json:"call_count,omitempty"`
	ErrorRate  float64   `json:"error_rate,omitempty"`
	AvgMs      float64   `json:"avg_latency_ms,omitempty"`
	TotalMs    float64   `json:"-"`
	ErrorCount int64     `json:"-"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// --- Query Result Types ---

// --- Causal-analysis quality contract (#163) ---

// Coverage source values. They are part of the MCP response contract: a client
// reads Source to know whether an answer is a fact about a complete retained
// exemplar, a fact about a deliberately truncated one, or an admission that
// nothing was retained.
const (
	// CoverageRetained — the complete parent/child chain was retained.
	CoverageRetained = "retained_exemplar"
	// CoveragePartial — the exemplar exists but its chain is incomplete:
	// truncated by the per-trace span/byte bound, or still arriving, or aged
	// out of the in-memory trace TTL.
	CoveragePartial = "partial_exemplar"
	// CoverageNone — no exemplar. The honest answer is "not retained or not
	// found", never a chain assembled from whatever happened to be around.
	CoverageNone = "not_retained_or_not_found"
)

// Coverage states what a causal-analysis answer is actually backed by.
//
// Errors are always ELIGIBLE for retention; they are never all PERSISTED
// (#161, #163). ErrorChain, RootCauseAnalysis, DependencyChain and the
// trace_graph tool are guaranteed only for complete retained exemplars. A
// truncated exemplar reports explicit partial coverage and an absent one says
// so — fabricated certainty is the one answer that is never acceptable.
type Coverage struct {
	// Complete is true only for a chain that reaches a root span with every
	// parent link resolved.
	Complete bool `json:"complete"`
	// Truncated marks a chain cut short by the per-trace span or byte bound.
	Truncated bool `json:"truncated"`
	// RetainedSpans and ObservedSpans are the retained/observed counts when
	// they are known.
	RetainedSpans int `json:"retained_spans,omitempty"`
	ObservedSpans int `json:"observed_spans,omitempty"`
	// Source is one of the Coverage* constants.
	Source string `json:"source"`
	// Note explains a non-complete answer in plain words.
	Note string `json:"note,omitempty"`
}

// ErrorChainResult is the output of an error chain query.
type ErrorChainResult struct {
	RootCause        *RootCauseInfo   `json:"root_cause"`
	SpanChain        []SpanNode       `json:"span_chain"`
	CorrelatedLogs   []LogClusterNode `json:"correlated_logs,omitempty"`
	AnomalousMetrics []MetricNode     `json:"anomalous_metrics,omitempty"`
	TraceID          string           `json:"trace_id"`
	// Coverage states whether this chain is backed by a complete retained
	// exemplar. A chain whose upstream walk stopped at a missing parent is
	// reported as partial, not presented as a root cause.
	Coverage Coverage `json:"coverage"`
}

// TraceGraphResult is the trace_graph tool's response shape: the retained span
// tree plus an explicit statement of what it covers.
type TraceGraphResult struct {
	TraceID  string     `json:"trace_id"`
	Spans    []SpanNode `json:"spans"`
	Coverage Coverage   `json:"coverage"`
}

// RootCauseInfo identifies the responsible service and operation.
type RootCauseInfo struct {
	Service      string `json:"service"`
	Operation    string `json:"operation"`
	ErrorMessage string `json:"error_message"`
	SpanID       string `json:"span_id"`
	TraceID      string `json:"trace_id"`
}

// ImpactResult describes the blast radius of a service failure.
type ImpactResult struct {
	Service          string          `json:"service"`
	AffectedServices []AffectedEntry `json:"affected_services"`
	TotalDownstream  int             `json:"total_downstream"`
}

// AffectedEntry is a service affected by an upstream failure.
type AffectedEntry struct {
	Service     string  `json:"service"`
	Depth       int     `json:"depth"`
	CallCount   int64   `json:"call_count"`
	ImpactScore float64 `json:"impact_score"`
}

// RankedCause is a probable root cause with evidence.
type RankedCause struct {
	Service    string        `json:"service"`
	Operation  string        `json:"operation"`
	Score      float64       `json:"score"`
	Evidence   []string      `json:"evidence"`
	ErrorChain []SpanNode    `json:"error_chain,omitempty"`
	Anomalies  []AnomalyNode `json:"anomalies,omitempty"`
	// Coverage states what the ranking rests on: a complete retained
	// exemplar, a partial one, or aggregate anomaly evidence with no chain.
	Coverage Coverage `json:"coverage"`
}

// --- Drain Template Persistence ---

// DrainTemplateRow is the persisted GORM representation of a Drain log
// template. Tokens are JSON-encoded to stay schema-simple across SQLite/
// MySQL/PostgreSQL/MSSQL.
//
// ID is stored as int64 (bit-reinterpretation of the uint64 FNV-64 hash): the
// standard SQL drivers reject uint64 values with the high bit set, and signed
// int64 carries the same 64 bits without loss. Conversion happens in the
// persistence helpers.
//
// The primary key is composite (tenant_id, id): the same template tokens can
// legitimately recur across tenants, and we want the cluster ID to stay stable
// per tenant once the in-memory Drain miner is partitioned per-tenant. TenantID
// is declared first so it leads the PK index.
type DrainTemplateRow struct {
	TenantID  string    `gorm:"primaryKey;size:64;default:'default';not null" json:"tenant_id"`
	ID        int64     `gorm:"primaryKey;autoIncrement:false" json:"id"` // int64(Template.ID)
	Tokens    string    `gorm:"type:text;not null" json:"tokens"`         // JSON-encoded []string
	Count     int       `json:"count"`
	FirstSeen time.Time `gorm:"index" json:"first_seen"`
	LastSeen  time.Time `gorm:"index" json:"last_seen"`
	Sample    string    `gorm:"type:text" json:"sample"`
}

// TableName overrides GORM's default table name.
func (DrainTemplateRow) TableName() string { return "drain_templates" }
