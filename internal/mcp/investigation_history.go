package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// investigationHistory supplements recent inference with durable evidence.
// It does not relabel saved probable causes as proven roots or recompute
// anomaly detection across historical aggregate buckets.
func (s *Server) investigationHistory(ctx context.Context, result ToolCallResult, service string, since, end time.Time) ToolCallResult {
	if result.IsError {
		return result
	}
	traceStart := end.Add(-s.traceHorizon)
	anomalyStart := end.Add(-24 * time.Hour)
	if anomalyStart.Before(s.startedAt) {
		anomalyStart = s.startedAt
	}
	evidence := map[string]any{
		"coverage": "partial", "complete": false,
		"requested":                timeWindow{since, end},
		"in_memory_trace_window":   timeWindow{traceStart, end},
		"in_memory_anomaly_window": timeWindow{anomalyStart, end},
		"coverage_note":            "in-memory evidence is bounded by TTL, process lifetime and sampling; saved investigations are selected leads, not a complete anomaly timeline or proof of root cause. Use get_service_map/get_service_health ranges, get_log_summary, search_logs and trace_graph to inspect retained history",
		"persisted_investigations": []graphrag.Investigation{},
		"persisted_limit":          50,
	}
	if s.repo != nil && s.repo.DB().WithContext(ctx).Migrator().HasTable(&graphrag.Investigation{}) {
		var investigations []graphrag.Investigation
		q := s.repo.DB().WithContext(ctx).Where("tenant_id = ? AND created_at >= ? AND created_at <= ?", storage.TenantFromContext(mcpCtx(ctx)), since, end)
		if service != "" {
			q = q.Where("(trigger_service = ? OR root_service = ?)", service, service)
		}
		err := q.Order("created_at DESC").Limit(51).Find(&investigations).Error
		if err != nil {
			return errorResult(fmt.Sprintf("persisted investigation read failed: %v", err))
		}
		evidence["persisted_truncated"] = len(investigations) > 50
		if len(investigations) > 50 {
			investigations = investigations[:50]
		}
		if investigations == nil {
			investigations = []graphrag.Investigation{}
		}
		evidence["persisted_investigations"] = investigations
		evidence["persisted_available"] = true
	} else {
		evidence["persisted_available"] = false
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return errorResult(err.Error())
	}
	size := len(data)
	for _, item := range result.Content {
		size += len(item.Text)
		if item.Resource != nil {
			size += len(item.Resource.Text)
		}
	}
	if size > MaxToolResponseBytes {
		return errorResult("investigation response too large; narrow the requested range")
	}
	result.Content = append(result.Content, ContentItem{Type: "text", Text: string(data)})
	return result
}
