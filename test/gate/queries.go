//go:build gate

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

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
	c := gatecore.QueryCheck{Name: spec.Name, CoverageExpected: spec.ExpectCoverage}

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
	if spec.Name == "service_map_seven_day" {
		object, objectOK := decoded.(map[string]any)
		nodes, nodesOK := object["nodes"].([]any)
		if !objectOK || !nodesOK {
			c.Error = "service-map response does not carry a top-level nodes array"
			return c
		}
		if c.Scalars == nil {
			c.Scalars = map[string]float64{}
		}
		c.Scalars["services"] = float64(len(nodes))
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
		Result struct {
			Content []struct {
				Text     string `json:"text"`
				Resource *struct {
					Text string `json:"text"`
				} `json:"resource"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
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
	if len(envelope.Result.Content) > 0 {
		call.PrimaryText = envelope.Result.Content[0].Text
		if call.PrimaryText == "" && envelope.Result.Content[0].Resource != nil {
			call.PrimaryText = envelope.Result.Content[0].Resource.Text
		}
	}
	call.TruncatedFound, call.TruncatedTrue = gatecore.ScanTruncated(envelope.Result)
	return call
}

func (g *gate) runQueryLatencyChecks() []gatecore.QueryLatencyCheck {
	checks := []struct {
		name string
		path string
		warm int
	}{
		{"default_dashboard", "/api/metrics/dashboard", 20},
		{"system_graph", "/api/system/graph", 20},
		{"live", "/live", 0},
		{"ready", "/ready", 0},
	}
	out := make([]gatecore.QueryLatencyCheck, 0, len(checks))
	for _, spec := range checks {
		check := gatecore.QueryLatencyCheck{Name: spec.name, URL: g.baseURL() + spec.path}
		_, status, header, elapsed, err := g.get(check.URL)
		check.Status, check.ColdSeconds = status, elapsed.Seconds()
		if header != nil {
			check.ColdCache = header.Get("X-Cache")
		}
		if err != nil || status != http.StatusOK {
			if err != nil {
				check.Error = err.Error()
			} else {
				check.Error = fmt.Sprintf("HTTP %d", status)
			}
			out = append(out, check)
			continue
		}
		for i := 0; i < spec.warm; i++ {
			_, warmStatus, warmHeader, warmElapsed, warmErr := g.get(check.URL)
			if warmErr != nil || warmStatus != http.StatusOK {
				if warmErr != nil {
					check.Error = warmErr.Error()
				} else {
					check.Error = fmt.Sprintf("warm request %d: HTTP %d", i+1, warmStatus)
				}
				break
			}
			if warmHeader.Get("X-Cache") == "HIT" {
				check.WarmCacheHits++
			}
			check.WarmSeconds = append(check.WarmSeconds, warmElapsed.Seconds())
		}
		check.WarmP50 = nearestRank(check.WarmSeconds, 0.50)
		check.WarmP95 = nearestRank(check.WarmSeconds, 0.95)
		check.WarmMax = nearestRank(check.WarmSeconds, 1)
		out = append(out, check)
	}
	return out
}

// waitLatencySentinel keeps the async ingest pipeline from turning a valid
// fixture into a timing race. Each probe has a unique ignored query parameter,
// so a zero-result response cannot poison the dashboard response cache.
func (g *gate) waitLatencySentinel(service string, want uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := "no response"
	for time.Now().Before(deadline) {
		q := url.Values{
			"service_name": {service},
			"_gate_wait":   {strconv.FormatInt(time.Now().UnixNano(), 10)},
		}
		u := g.baseURL() + "/api/metrics/dashboard?" + q.Encode()
		remaining := time.Until(deadline)
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		body, status, _, _, err := g.doGet(g.ctl, ctx, u)
		cancel()
		if err == nil && status == http.StatusOK {
			var payload struct {
				Provenance *latencyProvenanceWire `json:"latency_provenance"`
			}
			if decodeErr := json.Unmarshal(body, &payload); decodeErr != nil {
				last = decodeErr.Error()
			} else if payload.Provenance != nil && payload.Provenance.P99 != nil {
				got := payload.Provenance.P99.SampleCount
				if got == want {
					return nil
				}
				last = fmt.Sprintf("sample_count=%d, want %d", got, want)
			} else {
				last = "p99 latency provenance is absent"
			}
		} else {
			last = requestError(status, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("latency sentinel did not become query-visible within %s (last: %s)", timeout, last)
}

func nearestRank(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	idx := int(float64(len(sorted))*q+0.999999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

type latencyPercentileWire struct {
	Status             string  `json:"status"`
	Method             string  `json:"method"`
	SampleCount        uint64  `json:"sample_count"`
	SketchScale        uint8   `json:"sketch_scale"`
	RelativeErrorBound float64 `json:"relative_error_bound"`
	Degraded           bool    `json:"degraded"`
	Collapsed          bool    `json:"collapsed"`
	Saturations        uint64  `json:"saturations"`
}

type latencyProvenanceWire struct {
	P99 *latencyPercentileWire `json:"p99"`
}

type latencyServiceWire struct {
	Name              string                 `json:"name"`
	P99LatencyMS      float64                `json:"p99_latency_ms"`
	LatencyProvenance *latencyProvenanceWire `json:"latency_provenance"`
}

func latencySurface(name string, value float64, provenance *latencyProvenanceWire) gatecore.LatencySurface {
	surface := gatecore.LatencySurface{Name: name, ValueMS: value}
	if provenance == nil || provenance.P99 == nil {
		surface.Error = "p99 latency provenance is absent"
		return surface
	}
	p := provenance.P99
	surface.Status, surface.Method, surface.SampleCount = p.Status, p.Method, p.SampleCount
	surface.SketchScale, surface.RelativeErrorBound = p.SketchScale, p.RelativeErrorBound
	surface.Degraded, surface.Collapsed, surface.Saturations = p.Degraded, p.Collapsed, p.Saturations
	return surface
}

func (g *gate) collectLatencySentinel() []gatecore.LatencySurface {
	service := g.res.Queries.LatencySentinel.Service
	var out []gatecore.LatencySurface

	dashboardURL := g.baseURL() + "/api/metrics/dashboard?service_name=" + url.QueryEscape(service)
	if body, status, _, _, err := g.get(dashboardURL); err != nil || status != http.StatusOK {
		out = append(out, gatecore.LatencySurface{Name: "rest_dashboard", Error: requestError(status, err)})
	} else {
		var payload struct {
			P99LatencyMS float64                `json:"p99_latency_ms"`
			Provenance   *latencyProvenanceWire `json:"latency_provenance"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			out = append(out, gatecore.LatencySurface{Name: "rest_dashboard", Error: err.Error()})
		} else {
			out = append(out, latencySurface("rest_dashboard", payload.P99LatencyMS, payload.Provenance))
		}
	}

	graphURL := g.baseURL() + "/api/system/graph"
	if body, status, _, _, err := g.get(graphURL); err != nil || status != http.StatusOK {
		out = append(out, gatecore.LatencySurface{Name: "rest_system_graph", Error: requestError(status, err)})
	} else {
		var payload struct {
			Nodes []struct {
				ID      string `json:"id"`
				Metrics struct {
					P99LatencyMS float64                `json:"p99_latency_ms"`
					Provenance   *latencyProvenanceWire `json:"latency_provenance"`
				} `json:"metrics"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			out = append(out, gatecore.LatencySurface{Name: "rest_system_graph", Error: err.Error()})
		} else {
			found := false
			for _, node := range payload.Nodes {
				if node.ID == service {
					out = append(out, latencySurface("rest_system_graph", node.Metrics.P99LatencyMS, node.Metrics.Provenance))
					found = true
					break
				}
			}
			if !found {
				out = append(out, gatecore.LatencySurface{Name: "rest_system_graph", Error: "sentinel service is absent"})
			}
		}
	}

	out = append(out, g.websocketLatencySurface(service))
	mapCall := g.runMCPTool(gatecore.MCPToolSpec{Name: "get_service_map", Arguments: map[string]any{"service": service, "depth": 1}}, time.Time{}, time.Time{})
	mapSurface := latencySurfaceFromMCP("mcp_get_service_map", service, mapCall.PrimaryText)
	out = append(out, mapSurface)
	graphRAGSurface := mapSurface
	graphRAGSurface.Name = "graphrag_service"
	out = append(out, graphRAGSurface)
	healthCall := g.runMCPTool(gatecore.MCPToolSpec{Name: "get_service_health", Arguments: map[string]any{"service_name": service}}, time.Time{}, time.Time{})
	out = append(out, latencySurfaceFromMCP("mcp_get_service_health", service, healthCall.PrimaryText))
	return out
}

func (g *gate) websocketLatencySurface(service string) gatecore.LatencySurface {
	name := "websocket_dashboard"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	header := http.Header{}
	if g.cfg.APIKey != "" {
		header.Set("Authorization", "Bearer "+g.cfg.APIKey)
	}
	u := "ws" + strings.TrimPrefix(g.baseURL(), "http") + "/ws/events?service=" + url.QueryEscape(service)
	conn, response, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: header})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return gatecore.LatencySurface{Name: name, Error: err.Error()}
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "gate proof complete") }()
	_, body, err := conn.Read(ctx)
	if err != nil {
		return gatecore.LatencySurface{Name: name, Error: err.Error()}
	}
	var payload struct {
		Dashboard *struct {
			P99Micros  int64                  `json:"p99_latency"`
			Provenance *latencyProvenanceWire `json:"latency_provenance"`
		} `json:"dashboard"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return gatecore.LatencySurface{Name: name, Error: err.Error()}
	}
	if payload.Dashboard == nil {
		return gatecore.LatencySurface{Name: name, Error: "dashboard is absent"}
	}
	return latencySurface(name, float64(payload.Dashboard.P99Micros)/1000, payload.Dashboard.Provenance)
}

func latencySurfaceFromMCP(name, service, text string) gatecore.LatencySurface {
	if text == "" {
		return gatecore.LatencySurface{Name: name, Error: "MCP primary result text is absent"}
	}
	var entries []struct {
		Service *latencyServiceWire `json:"service"`
	}
	if strings.HasPrefix(strings.TrimSpace(text), "[") {
		if err := json.Unmarshal([]byte(text), &entries); err != nil {
			return gatecore.LatencySurface{Name: name, Error: err.Error()}
		}
	} else {
		var entry struct {
			Service *latencyServiceWire `json:"service"`
		}
		if err := json.Unmarshal([]byte(text), &entry); err != nil {
			return gatecore.LatencySurface{Name: name, Error: err.Error()}
		}
		entries = append(entries, entry)
	}
	for _, entry := range entries {
		if entry.Service != nil && entry.Service.Name == service {
			return latencySurface(name, entry.Service.P99LatencyMS, entry.Service.LatencyProvenance)
		}
	}
	return gatecore.LatencySurface{Name: name, Error: "sentinel service is absent"}
}

func requestError(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", status)
}

// get performs an authorized GET on the query client (long timeout, for the
// completeness surfaces).
func (g *gate) get(u string) ([]byte, int, http.Header, time.Duration, error) {
	return g.doGet(g.http, context.Background(), u)
}

// doGet performs an authorized GET on an explicit client, bounded by ctx.
func (g *gate) doGet(c *http.Client, ctx context.Context, u string) ([]byte, int, http.Header, time.Duration, error) {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, nil, 0, err
	}
	g.authorize(req)
	resp, err := c.Do(req)
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

// waitReady polls /ready until it answers 200 or the deadline passes. Each
// request runs on the control client with a context bounded by the remaining
// overall deadline, so one hung request cannot outlive the loop's budget.
func (g *gate) waitReady(timeout time.Duration) (time.Time, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		body, status, _, _, err := g.doGet(g.ctl, ctx, g.baseURL()+"/ready")
		cancel()
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
