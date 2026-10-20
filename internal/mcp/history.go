package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// SetRawRetention sets the raw exemplar search horizon without changing global
// retention defaults. Call at startup with the effective raw retention.
func (s *Server) SetRawRetention(d time.Duration) {
	if d > 0 {
		s.rawRetention = d
	}
}

// SetHistoryRetention sets the aggregate investigation horizon at startup.
func (s *Server) SetHistoryRetention(d time.Duration) {
	if d > 0 {
		s.historyRetention = min(d, 7*24*time.Hour)
	}
}

// SetTraceHorizon sets the configured GraphRAG trace TTL used in coverage notes.
func (s *Server) SetTraceHorizon(d time.Duration) {
	if d > 0 {
		s.traceHorizon = d
	}
}

type timeWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}
type rangeCoverage struct {
	Requested timeWindow `json:"requested"`
	Available timeWindow `json:"available"`
	Queried   timeWindow `json:"queried"`
	Coverage  string     `json:"coverage"`
	Note      string     `json:"coverage_note"`
	Clamped   bool       `json:"clamped"`
}

func strictTime(args map[string]any, key string) (time.Time, error) {
	v, exists := args[key]
	if !exists {
		return time.Time{}, nil
	}
	str, ok := v.(string)
	if !ok || str == "" {
		return time.Time{}, fmt.Errorf("%s must be RFC3339", key)
	}
	ts, err := time.Parse(time.RFC3339Nano, str)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339", key)
	}
	return ts, nil
}
func boundedInt(args map[string]any, key string, def, lo, hi int) (int, error) {
	raw, exists := args[key]
	if !exists {
		return def, nil
	}
	var n float64
	switch v := raw.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < float64(lo) || n > float64(hi) {
		return 0, fmt.Errorf("%s must be between %d and %d", key, lo, hi)
	}
	return int(n), nil
}

// retentionWindow bounds SQL work and makes any retention clamp visible. The
// available interval is a policy boundary, never a claim of continuous data.
func retentionWindow(args map[string]any, retention, defaultLookback time.Duration) (rangeCoverage, error) {
	now := time.Now().UTC()
	c := rangeCoverage{Available: timeWindow{now.Add(-retention), now}}
	start, err := strictTime(args, "start")
	if err != nil {
		return c, err
	}
	end, err := strictTime(args, "end")
	if err != nil {
		return c, err
	}
	if end.IsZero() {
		end = now
	}
	if start.IsZero() {
		start = end.Add(-min(defaultLookback, retention))
	}
	c.Requested = timeWindow{start, end}
	if !start.Before(end) {
		return c, fmt.Errorf("start must be before end")
	}
	if end.Sub(start) > retention {
		return c, fmt.Errorf("requested range exceeds configured retention of %s", retention)
	}
	if start.Before(c.Available.Start) {
		start = c.Available.Start
	}
	if end.After(now) {
		end = now
	}
	if !start.Before(end) {
		return c, fmt.Errorf("requested range is outside configured retention of %s", retention)
	}
	c.Queried = timeWindow{start, end}
	c.Clamped = !start.Equal(c.Requested.Start) || !end.Equal(c.Requested.End)
	return c, nil
}

func (s *Server) toolSearchLogs(ctx context.Context, args map[string]any) ToolCallResult {
	c, err := retentionWindow(args, s.rawRetention, 24*time.Hour)
	if err != nil {
		return errorResult(err.Error())
	}
	limit, err := boundedInt(args, "limit", 50, 1, 200)
	if err != nil {
		return errorResult(err.Error())
	}
	page, err := boundedInt(args, "page", 0, 0, 10000/limit)
	if err != nil {
		return errorResult(err.Error())
	}
	filter := storage.LogFilter{StartTime: c.Queried.Start, EndTime: c.Queried.End, Limit: limit, Offset: page * limit}
	for key, dest := range map[string]*string{"service": &filter.ServiceName, "severity": &filter.Severity, "query": &filter.Search, "trace_id": &filter.TraceID} {
		if raw, ok := args[key]; ok {
			value, ok := raw.(string)
			if !ok || len(value) > 4096 {
				return errorResult(key + " must be a string of at most 4096 bytes")
			}
			*dest = value
		}
	}
	if c.Queried.End.Sub(c.Queried.Start) > 24*time.Hour && filter.ServiceName == "" && filter.Severity == "" && filter.TraceID == "" {
		return errorResult("searches over 24h require service, severity or trace_id; use get_log_summary for unscoped history")
	}
	if s.repo == nil {
		return errorResult("log repository unavailable")
	}
	logs, total, err := s.repo.GetLogsV2(mcpCtx(ctx), filter)
	if err != nil {
		return errorResult(fmt.Sprintf("search_logs failed: %v", err))
	}
	c.Coverage = "retained_raw"
	c.Note = "available is the configured retention boundary, not continuous data coverage; results only describe retained rows"
	if s.aggregateMode {
		c.Coverage = "exemplar"
		c.Note += "; raw logs are sampled and missing rows do not prove zero events"
	}
	data, err := json.Marshal(map[string]any{"total": total, "page": page, "limit": limit, "count": len(logs), "entries": toLogSummaries(logs), "coverage": c})
	if err != nil {
		return errorResult(err.Error())
	}
	return resourceResult(resourceURIPrefix+"logs/search", httpconst.ContentTypeJSON, string(data))
}

type LogSummaryQuerier interface {
	QueryLogSummary(context.Context, aggregate.Query) (*aggregate.LogSummaryResult, error)
}

// SetLogSummaryQuerier injects the aggregate engine at startup in aggregate mode.
func (s *Server) SetLogSummaryQuerier(q LogSummaryQuerier) { s.logSummary = q }
func (s *Server) toolLogSummary(ctx context.Context, args map[string]any) ToolCallResult {
	if s.logSummary == nil || !s.aggregateMode {
		return errorResult("aggregate log summary unavailable in this mode")
	}
	c, err := retentionWindow(args, s.historyRetention, 24*time.Hour)
	if err != nil {
		return errorResult(err.Error())
	}
	q := aggregate.Query{Tenant: storage.TenantFromContext(mcpCtx(ctx)), Start: c.Queried.Start, End: c.Queried.End}
	if raw, ok := args["service"]; ok {
		service, ok := raw.(string)
		if !ok || len(service) > 255 {
			return errorResult("service must be a string of at most 255 bytes")
		}
		if service != "" {
			q.Services = []string{service}
		}
	}
	summary, err := s.logSummary.QueryLogSummary(ctx, q)
	if err != nil {
		return errorResult(fmt.Sprintf("log summary failed: %v", err))
	}
	c.Coverage = string(summary.Coverage)
	c.Note = summary.Coverage.Note() + " Counts cover accepted logs in five-minute buckets, including unsampled bodies; available is configured retention, not proof of uninterrupted collection."
	data, err := json.Marshal(map[string]any{"summary": summary, "coverage": c})
	if err != nil {
		return errorResult(err.Error())
	}
	return textResult(string(data))
}
