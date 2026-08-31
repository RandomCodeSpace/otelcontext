// Package topology owns the mode-selected service-topology read contract.
package topology

import (
	"context"
	"strconv"
	"time"
)

// Source identifies the producer that owns a snapshot.
type Source string

const (
	SourceLegacy    Source = "legacy"
	SourceAggregate Source = "aggregate"
)

// Identity orders full-replacement snapshots within one process generation.
type Identity struct {
	Epoch    string `json:"epoch,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
}

// String returns the stable cache and stream identity.
func (i Identity) String() string {
	if i.Epoch == "" && i.Revision == 0 {
		return ""
	}
	return i.Epoch + ":" + strconv.FormatUint(i.Revision, 10)
}

// Query selects a range and an optional closed set of services. A zero range
// asks for the provider's current live replacement.
type Query struct {
	Start    time.Time
	End      time.Time
	Services []string
}

// Metadata is additive ownership and completeness information.
type Metadata struct {
	Source       Source    `json:"source,omitempty"`
	Start        time.Time `json:"start,omitempty"`
	End          time.Time `json:"end,omitempty"`
	Coverage     string    `json:"coverage,omitempty"`
	CoverageNote string    `json:"coverage_note,omitempty"`
	Epoch        string    `json:"epoch,omitempty"`
	Revision     uint64    `json:"revision,omitempty"`
	Truncated    bool      `json:"truncated,omitempty"`

	DroppedServices   uint64 `json:"dropped_services,omitempty"`
	DroppedOperations uint64 `json:"dropped_operations,omitempty"`
	DroppedEdges      uint64 `json:"dropped_edges,omitempty"`
	DroppedMetrics    uint64 `json:"dropped_metrics,omitempty"`
}

// Identity returns the ordering pair carried by this metadata.
func (m Metadata) Identity() Identity {
	return Identity{Epoch: m.Epoch, Revision: m.Revision}
}

// Node is the wire-neutral service projection shared by all consumers.
type Node struct {
	Name         string  `json:"name"`
	TotalTraces  int64   `json:"total_traces"`
	ErrorCount   int64   `json:"error_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`

	RequestRateRPS float64  `json:"request_rate_rps,omitempty"`
	ErrorRate      float64  `json:"error_rate,omitempty"`
	P99LatencyMs   float64  `json:"p99_latency_ms,omitempty"`
	SpanCount      int64    `json:"span_count,omitempty"`
	HealthScore    float64  `json:"health_score,omitempty"`
	Status         string   `json:"status,omitempty"`
	Alerts         []string `json:"alerts,omitempty"`
}

// Edge is one directed service dependency.
type Edge struct {
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	CallCount    int64   `json:"call_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
	Status       string  `json:"status,omitempty"`
}

// Snapshot is always a full replacement. Nodes and Edges are never nil.
type Snapshot struct {
	Nodes []Node   `json:"nodes"`
	Edges []Edge   `json:"edges"`
	Meta  Metadata `json:"meta,omitempty"`
}

// Provider is selected once from AGGREGATE_MODE and injected into every live
// topology consumer.
type Provider interface {
	Source() Source
	Identity(context.Context) Identity
	Snapshot(context.Context, Query) (Snapshot, error)
}
