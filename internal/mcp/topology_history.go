package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

func (s *Server) useTopologySnapshot(args map[string]any) bool {
	_, start := args["start"]
	_, end := args["end"]
	return start || end || s.topology != nil && (s.graphRAG == nil || s.aggregateMode && s.topology.Source() == topology.SourceAggregate)
}
func (s *Server) topologyTool(ctx context.Context, args map[string]any, health bool) ToolCallResult {
	if s.topology == nil {
		return errorResult("historical topology provider unavailable")
	}
	q := topology.Query{}
	_, hasStart := args["start"]
	_, hasEnd := args["end"]
	var requested *rangeCoverage
	if hasStart || hasEnd {
		c, err := retentionWindow(args, s.historyRetention, time.Hour)
		if err != nil {
			return errorResult(err.Error())
		}
		q.Start, q.End = c.Queried.Start, c.Queried.End
		requested = &c
	}
	depth, err := boundedInt(args, "depth", 3, 1, 20)
	if err != nil {
		return errorResult(err.Error())
	}
	focus, _ := args["service"].(string)
	if health {
		var ok bool
		focus, ok = args["service_name"].(string)
		if !ok || focus == "" {
			return errorResult("service_name is required")
		}
		// Include neighbors when rendering health dependencies, then select the
		// requested service from the snapshot. A closed single-service query would
		// discard every cross-service edge.
	}
	if len(focus) > 255 {
		return errorResult("service name exceeds 255 bytes")
	}
	snapshot, err := s.topology.Snapshot(mcpCtx(ctx), q)
	if err != nil {
		return errorResult(fmt.Sprintf("topology provider unavailable: %v", err))
	}
	entries := snapshotEntries(snapshot)
	if requested == nil && s.graphRAG != nil {
		operations := map[string][]*graphrag.OperationNode{}
		for _, entry := range s.graphRAG.ServiceMap(mcpCtx(ctx), 0) {
			if entry.Service != nil {
				operations[entry.Service.Name] = entry.Operations
			}
		}
		for i := range entries {
			entries[i].Operations = operations[entries[i].Service.Name]
		}
	}
	var payload any
	if health {
		payload = fmt.Sprintf("service %q not found in the requested tenant window", focus)
		for _, entry := range entries {
			if entry.Service.Name == focus {
				_, hosts := s.hosts(ctx).ServiceHosts(focus)
				payload = serviceHealthResult{ServiceMapEntry: entry, Hosts: hosts}
				break
			}
		}
	} else {
		if focus != "" {
			entries = entriesAround(entries, focus, depth)
		}
		payload = entries
		switch group, _ := args["group_by"].(string); group {
		case "":
		case "host":
			payload = serviceMapByHost{Services: entries, Hosts: s.hosts(ctx).Hosts}
		default:
			return errorResult("unsupported group_by (only host)")
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return errorResult(err.Error())
	}
	result := textResult(string(data))
	if result.IsError {
		return result
	}
	// The metadata comes from exactly the snapshot rendered above. Do not issue
	// another live query to describe a historical payload.
	meta := snapshot.Meta
	if meta.Coverage == "" {
		meta.Coverage = "retained_raw"
		meta.CoverageNote = "computed from retained rows; continuous collection is not proven"
	}
	if requested != nil {
		requested.Coverage = meta.Coverage
		requested.Note = meta.CoverageNote + " Available is the configured retention boundary, not proof of uninterrupted collection."
	}
	details := struct {
		topology.Metadata
		Range              *rangeCoverage `json:"range,omitempty"`
		HostCoverage       string         `json:"host_coverage"`
		OperationsCoverage string         `json:"operations_coverage"`
	}{Metadata: meta, Range: requested, HostCoverage: "current resource registry, not historical host placement", OperationsCoverage: "operation details are not included in historical topology snapshots"}
	extra, _ := json.Marshal(details)
	result.Content = append(result.Content, ContentItem{Type: "text", Text: string(extra)})
	return result
}
func snapshotEntries(snapshot topology.Snapshot) []graphrag.ServiceMapEntry {
	entries := make([]graphrag.ServiceMapEntry, 0, len(snapshot.Nodes))
	index := map[string]int{}
	for _, n := range snapshot.Nodes {
		index[n.Name] = len(entries)
		entries = append(entries, graphrag.ServiceMapEntry{Service: &graphrag.ServiceNode{ID: n.Name, Name: n.Name, CallCount: n.TotalTraces, ErrorCount: n.ErrorCount, ErrorRate: n.ErrorRate, AvgLatency: n.AvgLatencyMs, P99Latency: n.P99LatencyMs, LatencyProvenance: n.LatencyProvenance, HealthScore: n.HealthScore}})
	}
	for _, e := range snapshot.Edges {
		edge := &graphrag.Edge{Type: graphrag.EdgeCalls, FromID: e.Source, ToID: e.Target, CallCount: e.CallCount, AvgMs: e.AvgLatencyMs, ErrorRate: e.ErrorRate}
		if i, ok := index[e.Source]; ok {
			entries[i].CallsTo = append(entries[i].CallsTo, edge)
		}
		if i, ok := index[e.Target]; ok {
			entries[i].CalledBy = append(entries[i].CalledBy, edge)
		}
	}
	return entries
}
func entriesAround(entries []graphrag.ServiceMapEntry, focus string, depth int) []graphrag.ServiceMapEntry {
	index := map[string]graphrag.ServiceMapEntry{}
	for _, e := range entries {
		index[e.Service.Name] = e
	}
	seen := map[string]bool{focus: true}
	frontier := []string{focus}
	for i := 0; i < depth && len(frontier) > 0; i++ {
		next := []string{}
		for _, name := range frontier {
			e := index[name]
			for _, edges := range [][]*graphrag.Edge{e.CallsTo, e.CalledBy} {
				for _, edge := range edges {
					for _, neighbor := range []string{edge.FromID, edge.ToID} {
						if !seen[neighbor] {
							seen[neighbor] = true
							next = append(next, neighbor)
						}
					}
				}
			}
		}
		frontier = next
	}
	out := []graphrag.ServiceMapEntry{}
	for _, e := range entries {
		if seen[e.Service.Name] {
			out = append(out, e)
		}
	}
	return out
}
