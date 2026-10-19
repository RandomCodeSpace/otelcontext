package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/prometheus/common/model"
)

// PromQLQuerier is implemented by the tenant-bound query client. The raw
// envelope preserves result types, sample strings, warnings and backend errors.
type PromQLQuerier interface {
	Query(context.Context, url.Values, bool) (json.RawMessage, error)
}

// SetPromQLQuerier binds the client to its configured tenant at startup.
// Requests for any other tenant cannot reach that client.
func (s *Server) SetPromQLQuerier(q PromQLQuerier, tenant string) {
	if tenant == "" {
		tenant = s.defaultTenant
	}
	s.promQL, s.promQLTenant = q, tenant
}

func (s *Server) toolPromQLQuery(ctx context.Context, args map[string]any) ToolCallResult {
	return s.queryPromQL(ctx, args, false)
}
func (s *Server) toolPromQLRange(ctx context.Context, args map[string]any) ToolCallResult {
	return s.queryPromQL(ctx, args, true)
}
func (s *Server) queryPromQL(ctx context.Context, args map[string]any, ranged bool) ToolCallResult {
	if s.promQL == nil {
		return promQLError("unavailable", "PromQL backend is not configured")
	}
	ctx = mcpCtx(ctx)
	if storage.TenantFromContext(ctx) != s.promQLTenant {
		return promQLError("unavailable", "PromQL backend is not configured for this tenant")
	}
	params, err := promQLParams(args, ranged)
	if err != nil {
		return promQLError("bad_data", err.Error())
	}
	raw, err := s.promQL.Query(ctx, params, ranged)
	// A backend error envelope is useful evidence even when Query also returns
	// an error. Preserve it exactly rather than replacing it with an error string.
	var envelope struct {
		Status string `json:"status"`
	}
	if len(raw) <= MaxToolResponseBytes && json.Unmarshal(raw, &envelope) == nil && (envelope.Status == "success" || envelope.Status == "error") {
		res := textResult(string(raw))
		res.IsError = envelope.Status == "error" || err != nil
		return res
	}
	if err != nil {
		return promQLError("execution", err.Error())
	}
	if len(raw) > MaxToolResponseBytes {
		return promQLError("execution", "PromQL response exceeds MCP response-size limit")
	}
	return promQLError("execution", "PromQL backend returned an invalid response envelope")
}
func promQLError(kind, message string) ToolCallResult {
	data, _ := json.Marshal(map[string]string{"status": "error", "errorType": kind, "error": message})
	res := textResult(string(data))
	res.IsError = true
	return res
}

func promQLParams(args map[string]any, ranged bool) (url.Values, error) {
	allowed := map[string]bool{"query": true, "timeout": true, "limit": true}
	if ranged {
		allowed["start"], allowed["end"], allowed["step"] = true, true, true
	} else {
		allowed["time"] = true
	}
	params := url.Values{}
	for key, value := range args {
		if !allowed[key] {
			return nil, fmt.Errorf("unsupported PromQL argument %q", key)
		}
		if key == "limit" {
			continue
		}
		v, ok := value.(string)
		if !ok || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("%s must be a nonempty string", key)
		}
		params.Set(key, v)
	}
	if n := len(params.Get("query")); n == 0 || n > 16384 {
		return nil, fmt.Errorf("query must contain 1 to 16384 bytes")
	}
	limit, err := boundedInt(args, "limit", 1000, 1, 1000)
	if err != nil {
		return nil, err
	}
	params.Set("limit", strconv.Itoa(limit))
	if v := params.Get("timeout"); v != "" {
		d, e := model.ParseDuration(v)
		if e != nil || d <= 0 || time.Duration(d) > 30*time.Second {
			return nil, fmt.Errorf("timeout must be a positive duration at most 30s")
		}
	}
	if !ranged {
		if v := params.Get("time"); v != "" {
			if _, err := promTimestamp(v); err != nil {
				return nil, err
			}
		}
		return params, nil
	}
	start, err := promTimestamp(params.Get("start"))
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	end, err := promTimestamp(params.Get("end"))
	if err != nil {
		return nil, fmt.Errorf("end: %w", err)
	}
	if end < start || end-start > (7*24*time.Hour).Seconds() {
		return nil, fmt.Errorf("range must be ordered and at most 7 days")
	}
	step, err := strconv.ParseFloat(params.Get("step"), 64)
	if err != nil {
		d, e := model.ParseDuration(params.Get("step"))
		if e != nil {
			return nil, fmt.Errorf("step must be a positive duration or seconds")
		}
		step = time.Duration(d).Seconds()
	}
	if math.IsNaN(step) || math.IsInf(step, 0) || step <= 0 || (end-start)/step >= 11000 {
		return nil, fmt.Errorf("step must be positive and produce at most 11000 points per series")
	}
	return params, nil
}
func promTimestamp(v string) (float64, error) {
	if ts, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return float64(ts.Unix()) + float64(ts.Nanosecond())/1e9, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 253402300799 {
		return 0, fmt.Errorf("time must be RFC3339 or finite Unix seconds")
	}
	return n, nil
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
