//go:build gate

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/test/gate/gatecore"
)

// Query-completeness checks (Q3).
//
// Two surfaces are exercised: the HTTP query API over the full seven-day range,
// and the aggregate-backed MCP tools named explicitly in the gate config.

// runAPIChecks answers every configured HTTP surface.
func (g *gate) runAPIChecks(start, end time.Time, expectedWindows []int64) []gatecore.QueryCheck {
	out := make([]gatecore.QueryCheck, 0, len(g.cfg.Queries.API))
	for _, spec := range g.cfg.Queries.API {
		out = append(out, g.runAPICheck(spec, start, end, expectedWindows))
	}
	return out
}

func (g *gate) runAPICheck(spec gatecore.APICheck, sevenDayStart, sevenDayEnd time.Time, expectedWindows []int64) gatecore.QueryCheck {
	c := gatecore.QueryCheck{Name: spec.Name}

	u := g.baseURL() + spec.Path
	q := url.Values{}
	switch spec.Range {
	case "seven_day":
		q.Set("start", sevenDayStart.UTC().Format(time.RFC3339))
		q.Set("end", sevenDayEnd.UTC().Format(time.RFC3339))
	case "crash_run":
		q.Set("start", g.crashRunStart.UTC().Format(time.RFC3339))
		q.Set("end", g.crashRunEnd.UTC().Format(time.RFC3339))
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	c.URL = u

	body, status, header, dur, err := g.get(u)
	c.Status, c.DurationSec, c.BodyBytes = status, dur.Seconds(), len(body)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	if h := header.Get(aggregate.CoverageHeader); h != "" {
		c.Coverage, c.CoverageSource = h, "response header "+aggregate.CoverageHeader
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		c.Error = "response is not JSON: " + err.Error()
		return c
	}
	c.TruncatedFound, c.TruncatedTrue = gatecore.ScanTruncated(decoded)
	if c.Coverage == "" {
		if cov, ok := gatecore.FindStringField(decoded, "coverage"); ok {
			c.Coverage, c.CoverageSource = cov, "response body field"
		}
	}
	if len(spec.ScalarKeys) > 0 {
		c.Scalars = gatecore.TopLevelScalars(decoded, spec.ScalarKeys)
	}
	if spec.PerWindow && len(expectedWindows) > 0 {
		pts, perr := gatecore.ParseWindowPoints(body)
		if perr != nil {
			c.Error = perr.Error()
			return c
		}
		returned, missing, extra := gatecore.WindowCoverage(pts, expectedWindows, gatecore.WindowSecs)
		c.WindowsReturned, c.MissingWindows, c.WindowsExpected = returned, missing, len(expectedWindows)
		c.ExtraWindows = extra
	}
	// Whatever coverage marker arrived is always recorded; only surfaces the
	// config marks as required are gated on it.
	c.CoverageExpected = spec.ExpectCoverage
	return c
}

// windowTotals fetches per-window totals for one field over a range. It is the
// observation side of the crash-interval bound.
func (g *gate) windowTotals(start, end time.Time, field string) (map[int64]int64, error) {
	u := fmt.Sprintf("%s/api/metrics/traffic?start=%s&end=%s",
		g.baseURL(),
		url.QueryEscape(start.UTC().Format(time.RFC3339)),
		url.QueryEscape(end.UTC().Format(time.RFC3339)))
	body, status, _, _, err := g.get(u)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", u, status)
	}
	pts, err := gatecore.ParseWindowPoints(body)
	if err != nil {
		return nil, err
	}
	return gatecore.WindowTotals(pts, field, gatecore.WindowSecs), nil
}

// runMCPTools calls every explicitly named aggregate-backed tool over the
// seven-day range.
func (g *gate) runMCPTools(sevenDayStart, sevenDayEnd time.Time) []gatecore.MCPToolCall {
	out := make([]gatecore.MCPToolCall, 0, len(g.cfg.Queries.MCPTools))
	for _, spec := range g.cfg.Queries.MCPTools {
		out = append(out, g.runMCPTool(spec, sevenDayStart, sevenDayEnd))
	}
	return out
}

func (g *gate) runMCPTool(spec gatecore.MCPToolSpec, start, end time.Time) gatecore.MCPToolCall {
	call := gatecore.MCPToolCall{Tool: spec.Name}

	args := map[string]any{}
	for k, v := range spec.Arguments {
		args[k] = v
	}
	if spec.StartArg != "" {
		args[spec.StartArg] = start.UTC().Format(time.RFC3339)
	}
	if spec.EndArg != "" {
		args[spec.EndArg] = end.UTC().Format(time.RFC3339)
	}
	if spec.SinceArg != "" {
		args[spec.SinceArg] = start.UTC().Format(time.RFC3339)
	}
	argJSON, _ := json.Marshal(args)
	call.Arguments = string(argJSON)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": spec.Name, "arguments": args},
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		call.Error = err.Error()
		return call
	}

	started := time.Now()
	req, err := http.NewRequest(http.MethodPost, g.baseURL()+g.cfg.MCPPath, bytes.NewReader(reqBody))
	if err != nil {
		call.Error = err.Error()
		return call
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	g.authorize(req)

	resp, err := g.http.Do(req)
	call.DurationSec = time.Since(started).Seconds()
	if err != nil {
		call.Error = err.Error()
		return call
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		call.Error = err.Error()
		return call
	}
	call.Status, call.ResultBytes = resp.StatusCode, len(body)

	var envelope struct {
		Result any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		call.Error = "response is not JSON-RPC: " + err.Error()
		return call
	}
	if envelope.Error != nil {
		call.RPCError = fmt.Sprintf("%d %s", envelope.Error.Code, envelope.Error.Message)
		return call
	}
	call.TruncatedFound, call.TruncatedTrue = gatecore.ScanTruncated(envelope.Result)
	return call
}

// get performs an authorized GET and returns body, status, headers, duration.
func (g *gate) get(u string) ([]byte, int, http.Header, time.Duration, error) {
	started := time.Now()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, nil, 0, err
	}
	g.authorize(req)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, 0, nil, time.Since(started), err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, resp.Header, time.Since(started), err
}

// authorize stamps the bearer token when one is configured.
func (g *gate) authorize(req *http.Request) {
	if g.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)
	}
}

// waitReady polls /ready until it answers 200 or the deadline passes.
func (g *gate) waitReady(timeout time.Duration) (time.Time, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		body, status, _, _, err := g.get(g.baseURL() + "/ready")
		switch {
		case err != nil:
			last = err.Error()
		case status == http.StatusOK:
			return time.Now(), nil
		default:
			last = fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 400))
		}
		time.Sleep(500 * time.Millisecond)
	}
	return time.Time{}, fmt.Errorf("/ready did not pass within %s (last: %s)", timeout, last)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
