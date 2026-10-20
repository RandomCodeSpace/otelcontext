package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

type retainedSpan struct {
	graphrag.SpanNode
	EndTime    time.Time       `json:"end_time,omitempty"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
}

// Two 256 KiB payload budgets leave room below the 4 MiB response cap for
// worst-case JSON escaping (6x), 1,100 rows of framing, and coverage metadata.
var retainedTraceLimits = storage.TraceReadLimits{Spans: 1000, Logs: 100, SpanBytes: 256 << 10, LogBytes: 256 << 10}

type retainedTrace struct {
	TraceID       string            `json:"trace_id"`
	Spans         []retainedSpan    `json:"spans"`
	Logs          []logSummary      `json:"logs,omitempty"`
	Coverage      graphrag.Coverage `json:"coverage"`
	LogsTotal     int64             `json:"logs_total"`
	LogsTruncated bool              `json:"logs_truncated"`
	LogsNote      string            `json:"logs_note,omitempty"`
}

func (s *Server) toolTraceGraph(ctx context.Context, args map[string]any) ToolCallResult {
	id, ok := args["trace_id"].(string)
	if !ok || id == "" || len(id) > 128 {
		return errorResult("trace_id is required and must be at most 128 bytes")
	}
	ctx = mcpCtx(ctx)
	// Prefer the durable payload: the live graph can contain only the latest
	// child while older parents still exist on disk.
	result, found, err := s.traceGraphFromDB(ctx, id)
	if err != nil {
		return errorResult(fmt.Sprintf("trace_graph failed: %v", err))
	}
	if !found {
		result = retainedTrace{TraceID: id, Spans: []retainedSpan{}}
		if s.graphRAG != nil {
			for _, sp := range s.graphRAG.TraceGraph(ctx, id).Spans {
				result.Spans = append(result.Spans, retainedSpan{SpanNode: sp})
			}
		}
		result.Coverage = spanCoverage(result.Spans)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return errorResult(err.Error())
	}
	return resourceResult(resourceURIPrefix+"traces/"+id, httpconst.ContentTypeJSON, string(data))
}
func (s *Server) traceGraphFromDB(ctx context.Context, id string) (retainedTrace, bool, error) {
	if s.repo == nil {
		return retainedTrace{}, false, nil
	}
	trace, err := s.repo.GetTraceBounded(ctx, id, retainedTraceLimits)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return retainedTrace{}, false, nil
	}
	if err != nil {
		return retainedTrace{}, false, err
	}
	result := retainedTrace{TraceID: id, Spans: make([]retainedSpan, 0, len(trace.Spans)), Logs: toLogSummaries(trace.Logs), LogsTotal: trace.TotalLogs, LogsTruncated: trace.LogsTruncated}
	if trace.LogsTruncated {
		result.LogsNote = "correlated logs truncated by row or byte limits; use paginated search_logs with this trace_id and the trace time range for more retained log evidence"
	}
	for _, sp := range trace.Spans {
		attrs := json.RawMessage(sp.AttributesJSON)
		if len(attrs) > 0 && !json.Valid(attrs) {
			attrs, _ = json.Marshal(string(sp.AttributesJSON))
		}
		result.Spans = append(result.Spans, retainedSpan{SpanNode: graphrag.SpanNode{ID: sp.SpanID, TraceID: sp.TraceID, ParentSpanID: sp.ParentSpanID, Service: sp.ServiceName, Operation: sp.OperationName, Duration: float64(sp.Duration) / 1000, StatusCode: sp.Status, IsError: sp.Status == "STATUS_CODE_ERROR", Timestamp: sp.StartTime}, EndTime: sp.EndTime, Attributes: attrs})
	}
	sort.Slice(result.Spans, func(i, j int) bool {
		a, b := result.Spans[i], result.Spans[j]
		if a.Timestamp.Equal(b.Timestamp) {
			return a.ID < b.ID
		}
		return a.Timestamp.Before(b.Timestamp)
	})
	result.Coverage = spanCoverage(result.Spans)
	if trace.ObservedSpanCount != nil {
		result.Coverage.ObservedSpans = *trace.ObservedSpanCount
	}
	missingPayload := len(result.Spans) == 0 || (trace.RetainedSpanCount != nil && *trace.RetainedSpanCount > len(result.Spans)) || (trace.ObservedSpanCount != nil && *trace.ObservedSpanCount > len(result.Spans))
	if trace.SpansTruncated || missingPayload || (trace.Truncated != nil && *trace.Truncated) {
		result.Coverage.Complete = false
		result.Coverage.Truncated = true
		result.Coverage.Source = graphrag.CoveragePartial
		result.Coverage.Note = "persisted trace metadata or read limits indicate missing or truncated span payload; returned spans are the available retained evidence"
	}
	return result, true, nil
}

// Each parent walk must reach a root. Checking only that a parent ID exists
// would accept a disconnected cycle alongside a valid root as complete.
func spanCoverage(spans []retainedSpan) graphrag.Coverage {
	c := graphrag.Coverage{RetainedSpans: len(spans), Source: graphrag.CoverageNone, Note: "no retained exemplar found; this does not prove that no trace occurred"}
	if len(spans) == 0 {
		return c
	}
	c.Source = graphrag.CoverageRetained
	c.Complete = true
	c.Note = "complete parent chains among retained spans; unobserved spans and logs cannot be inferred"
	parents := make(map[string]string, len(spans))
	for _, sp := range spans {
		parents[sp.ID] = sp.ParentSpanID
		if sp.ID == "" {
			c.Complete = false
		}
	}
	resolved := map[string]bool{}
	for _, sp := range spans {
		path := map[string]bool{}
		id := sp.ID
		rooted := false
		for id != "" {
			if good, ok := resolved[id]; ok {
				rooted = good
				break
			}
			if path[id] {
				break
			}
			path[id] = true
			parent, ok := parents[id]
			if !ok {
				break
			}
			if parent == "" {
				rooted = true
				break
			}
			id = parent
		}
		for visited := range path {
			resolved[visited] = rooted
		}
		if !rooted {
			c.Complete = false
		}
	}
	if !c.Complete {
		c.Truncated = true
		c.Source = graphrag.CoveragePartial
		c.Note = "a retained parent chain is missing, cyclic or invalid; no complete trace can be inferred"
	}
	return c
}
