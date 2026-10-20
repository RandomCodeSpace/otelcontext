package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

type fakePromQL struct {
	params url.Values
	ranged bool
	tenant string
	calls  int
	raw    json.RawMessage
	err    error
	wait   bool
}

func (f *fakePromQL) Query(ctx context.Context, p url.Values, ranged bool) (json.RawMessage, error) {
	f.calls++
	f.params = p
	f.ranged = ranged
	f.tenant = storage.TenantFromContext(ctx)
	if f.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.raw, f.err
}
func rpcRequest(t *testing.T, srv *Server, method string, params any, tenant string) JSONRPCResponse {
	t.Helper()
	body, err := json.Marshal(JSONRPCRequest{JSONRPC: "2.0", ID: "test", Method: method, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	if tenant != "" {
		req.Header.Set(mcpTenantHeader, tenant)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var out JSONRPCResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func decodeRPCResult(t *testing.T, out JSONRPCResponse) ToolCallResult {
	t.Helper()
	if out.Error != nil {
		t.Fatalf("rpc error: %+v", out.Error)
	}
	data, _ := json.Marshal(out.Result)
	var result ToolCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestPromQLPreservesEnvelopeAndDoesNotCache(t *testing.T) {
	envelope := `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"service_name":"checkout","host_name":"host-a"},"value":[1792343000.125,"NaN"]}]},"warnings":["partial samples"],"infos":["test info"]}`
	backend := &fakePromQL{raw: json.RawMessage(envelope)}
	srv := New("alpha", nil, nil, nil)
	srv.SetPromQLQuerier(backend, "alpha")
	srv.SetAggregateMode(true)
	for i := 0; i < 2; i++ {
		res := decodeRPCResult(t, rpcRequest(t, srv, "tools/call", ToolCallParams{Name: "promql_query", Arguments: map[string]any{"query": "up"}}, ""))
		if res.IsError || len(res.Content) != 1 || res.Content[0].Text != envelope {
			t.Fatalf("envelope changed: %+v", res)
		}
	}
	if backend.calls != 2 || backend.tenant != "alpha" || backend.params.Get("limit") != "1000" {
		t.Fatalf("wrong execution: %+v", backend)
	}
	if srv.Stats().CacheHits != 0 {
		t.Fatal("time-relative PromQL was cached")
	}
	res := decodeRPCResult(t, rpcRequest(t, srv, "tools/call", ToolCallParams{Name: "promql_query", Arguments: map[string]any{"query": "up"}}, "beta"))
	if !res.IsError || backend.calls != 2 {
		t.Fatal("cross-tenant query reached bound backend")
	}
}
func TestPromQLRangeValidationAndErrors(t *testing.T) {
	backend := &fakePromQL{raw: json.RawMessage(`{"status":"success","data":{"resultType":"matrix","result":[]}}`)}
	srv := New("default", nil, nil, nil)
	srv.SetPromQLQuerier(backend, "")
	args := map[string]any{"query": "rate(http_requests_total[5m])", "start": "1791700000", "end": "1792304800", "step": "1m", "timeout": "5s", "limit": float64(25)}
	res := srv.toolHandler(context.Background(), "promql_query_range", args)
	if res.IsError || !backend.ranged || backend.params.Get("step") != "1m" || backend.params.Get("limit") != "25" {
		t.Fatalf("range forwarding: %+v %+v", res, backend)
	}
	for key, value := range map[string]any{"start": "NaN", "end": "Inf", "step": "0s", "timeout": "31s", "query": strings.Repeat("a", 16385), "limit": float64(0), "unknown": "x"} {
		bad := map[string]any{}
		for k, v := range args {
			bad[k] = v
		}
		bad[key] = value
		if r := srv.toolPromQLRange(context.Background(), bad); !r.IsError {
			t.Fatalf("accepted %s=%v", key, value)
		}
	}
	for _, step := range []string{"NaN", "Inf", "0.001"} {
		bad := map[string]any{}
		for k, v := range args {
			bad[k] = v
		}
		bad["step"] = step
		if r := srv.toolPromQLRange(context.Background(), bad); !r.IsError {
			t.Fatalf("accepted step %q", step)
		}
	}
	backend.raw = json.RawMessage(`{"status":"error","errorType":"bad_data","error":"parse error","warnings":["backend warning"]}`)
	backend.err = errors.New("HTTP 422")
	res = srv.toolPromQLQuery(context.Background(), map[string]any{"query": "bad("})
	if !res.IsError || res.Content[0].Text != string(backend.raw) {
		t.Fatalf("lost backend error envelope: %+v", res)
	}
	backend.raw = nil
	backend.err = errors.New("backend offline")
	res = srv.toolPromQLQuery(context.Background(), map[string]any{"query": "up"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, `"error":"backend offline"`) {
		t.Fatalf("lost transport error: %+v", res)
	}
}
func TestPromQLCallTimeoutAndConcurrency(t *testing.T) {
	backend := &fakePromQL{wait: true}
	srv := New("default", nil, nil, nil)
	srv.SetPromQLQuerier(backend, "")
	srv.SetCallTimeout(10 * time.Millisecond)
	out := rpcRequest(t, srv, "tools/call", ToolCallParams{Name: "promql_query", Arguments: map[string]any{"query": "up"}}, "")
	// The canceled client can return its structured error just before the
	// transport deadline wins. Both outcomes must remain errors.
	if out.Error != nil {
		if out.Error.Code != ErrCallTimeout {
			t.Fatalf("wrong timeout: %+v", out.Error)
		}
	} else if !decodeRPCResult(t, out).IsError {
		t.Fatal("timeout reported success")
	}
	deadline := time.Now().Add(time.Second)
	for srv.Stats().InFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.Stats().InFlight != 0 {
		t.Fatal("timed out call did not release its slot")
	}
	srv.SetCacheTTL(0)
	srv.SetCallLimit(1)
	srv.callSlots <- struct{}{}
	out = rpcRequest(t, srv, "tools/call", ToolCallParams{Name: "promql_query", Arguments: map[string]any{"query": "up"}}, "")
	if out.Error == nil || out.Error.Code != ErrServerOverloaded {
		t.Fatalf("concurrency not enforced: %+v", out)
	}
	<-srv.callSlots
}
func TestResourceListReadAndBounds(t *testing.T) {
	p := &rangeTopology{}
	srv := New("alpha", nil, nil, p)
	listed := rpcRequest(t, srv, "resources/list", nil, "")
	data, _ := json.Marshal(listed.Result)
	if strings.Contains(string(data), `"uri":"OtelContext://metrics/prometheus"`) {
		t.Fatal("unreadable scrape resource advertised")
	}
	out := rpcRequest(t, srv, "resources/read", map[string]any{"uri": resourceURIPrefix + "system/graph"}, "")
	if out.Error != nil {
		t.Fatalf("read failed: %+v", out.Error)
	}
	var body struct {
		Contents []Resource `json:"contents"`
	}
	data, _ = json.Marshal(out.Result)
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 1 || !strings.Contains(body.Contents[0].Text, "old-service") || p.tenants[0] != "alpha" {
		t.Fatalf("bad resource: %+v", body)
	}
	deadline := time.Now().Add(time.Second)
	for srv.Stats().InFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.Stats().InFlight != 0 {
		t.Fatal("timed out call did not release its slot")
	}
	srv.SetCacheTTL(0)
	srv.SetCallLimit(1)
	srv.callSlots <- struct{}{}
	overloaded := rpcRequest(t, srv, "resources/read", map[string]any{"uri": resourceURIPrefix + "system/graph"}, "")
	if overloaded.Error == nil || overloaded.Error.Code != ErrServerOverloaded {
		t.Fatalf("resource bypassed call cap: %+v", overloaded)
	}
	<-srv.callSlots
	unknown := rpcRequest(t, srv, "resources/read", map[string]any{"uri": "OtelContext://metrics/prometheus"}, "")
	if unknown.Error == nil {
		t.Fatal("unknown URI succeeded")
	}
	huge := &fakeMCPTopologyProvider{snapshot: topology.Snapshot{Nodes: []topology.Node{{Name: strings.Repeat("x", MaxToolResponseBytes+1)}}}}
	srv = New("alpha", nil, nil, huge)
	capped := rpcRequest(t, srv, "resources/read", map[string]any{"uri": resourceURIPrefix + "system/graph"}, "")
	if capped.Error == nil {
		t.Fatal("resource exceeded response cap")
	}
}

func TestPromQLRangeDoesNotCache(t *testing.T) {
	backend := &fakePromQL{raw: json.RawMessage(`{"status":"success","data":{"resultType":"matrix","result":[]}}`)}
	srv := New("default", nil, nil, nil)
	srv.SetPromQLQuerier(backend, "default")
	args := map[string]any{"query": "up", "start": "1791700000", "end": "1791700060", "step": "1s"}
	for i := 0; i < 2; i++ {
		res := decodeRPCResult(t, rpcRequest(t, srv, "tools/call", ToolCallParams{Name: "promql_query_range", Arguments: args}, ""))
		if res.IsError {
			t.Fatalf("range failed: %+v", res)
		}
	}
	if backend.calls != 2 || srv.Stats().CacheHits != 0 || srv.cache.Stats() != 0 {
		t.Fatalf("range query was cached: backend calls=%d stats=%+v", backend.calls, srv.Stats())
	}
}

func TestPromQLDiscoveryAndDisabledBackend(t *testing.T) {
	srv := New("default", nil, nil, nil)
	out := rpcRequest(t, srv, "tools/list", nil, "")
	data, _ := json.Marshal(out.Result)
	var listed struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(data, &listed); err != nil {
		t.Fatal(err)
	}
	if out.Error != nil || len(listed.Tools) != 10 {
		t.Fatalf("tool discovery: %+v", out)
	}
	for _, name := range []string{"promql_query", "promql_query_range"} {
		found := false
		for _, tool := range listed.Tools {
			if tool.Name == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing tool %s", name)
		}
		res := decodeRPCResult(t, rpcRequest(t, srv, "tools/call", ToolCallParams{Name: name}, ""))
		if !res.IsError || !strings.Contains(res.Content[0].Text, "not configured") {
			t.Fatalf("disabled %s: %+v", name, res)
		}
	}
}

type deadlineResourceTopology struct{ fakeMCPTopologyProvider }

func (*deadlineResourceTopology) Snapshot(ctx context.Context, _ topology.Query) (topology.Snapshot, error) {
	<-ctx.Done()
	return topology.Snapshot{}, ctx.Err()
}

func TestResourceReadRespectsDeadline(t *testing.T) {
	g := graphrag.New(nil, nil, nil, graphrag.DefaultConfig())
	srv := New("default", nil, nil, &deadlineResourceTopology{})
	srv.SetGraphRAG(g)
	srv.SetAggregateMode(true)
	srv.SetCallTimeout(10 * time.Millisecond)
	out := rpcRequest(t, srv, "resources/read", map[string]any{"uri": resourceURIPrefix + "system/graph"}, "")
	if out.Error == nil || (out.Error.Code != ErrCallTimeout && out.Error.Code != ErrInternalError) {
		t.Fatalf("resource deadline not enforced: %+v", out)
	}
	deadline := time.Now().Add(time.Second)
	for srv.Stats().InFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.Stats().InFlight != 0 {
		t.Fatal("resource deadline left slot held")
	}
}

func TestLegacyResourceListReadAndBounds(t *testing.T) {
	g := graphrag.New(nil, nil, nil, graphrag.DefaultConfig())
	g.ObserveSpanTopology("alpha", "trace", "span-alpha", "", "alpha-service")
	g.ObserveSpanTopology("beta", "trace", "span-beta", "", "beta-service")
	srv := New("alpha", nil, nil, nil)
	srv.SetGraphRAG(g)
	listed := rpcRequest(t, srv, "resources/list", nil, "")
	data, _ := json.Marshal(listed.Result)
	if strings.Contains(string(data), `"uri":"OtelContext://metrics/prometheus"`) {
		t.Fatal("unreadable scrape resource advertised")
	}
	out := rpcRequest(t, srv, "resources/read", map[string]any{"uri": resourceURIPrefix + "system/graph"}, "")
	if out.Error != nil {
		t.Fatalf("read failed: %+v", out.Error)
	}
	var body struct {
		Contents []Resource `json:"contents"`
	}
	data, _ = json.Marshal(out.Result)
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 1 || !strings.Contains(body.Contents[0].Text, "alpha-service") || strings.Contains(body.Contents[0].Text, "beta-service") {
		t.Fatalf("bad resource: %+v", body)
	}
	deadline := time.Now().Add(time.Second)
	for srv.Stats().InFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.Stats().InFlight != 0 {
		t.Fatal("timed out call did not release its slot")
	}
	srv.SetCacheTTL(0)
	srv.SetCallLimit(1)
	srv.callSlots <- struct{}{}
	overloaded := rpcRequest(t, srv, "resources/read", map[string]any{"uri": resourceURIPrefix + "system/graph"}, "")
	if overloaded.Error == nil || overloaded.Error.Code != ErrServerOverloaded {
		t.Fatalf("resource bypassed call cap: %+v", overloaded)
	}
	<-srv.callSlots
	unknown := rpcRequest(t, srv, "resources/read", map[string]any{"uri": "OtelContext://metrics/prometheus"}, "")
	if unknown.Error == nil {
		t.Fatal("unknown URI succeeded")
	}
	g.ObserveSpanTopology("alpha", "trace", "span-huge", "", strings.Repeat("x", MaxToolResponseBytes+1))
	srv = New("alpha", nil, nil, nil)
	srv.SetGraphRAG(g)
	capped := rpcRequest(t, srv, "resources/read", map[string]any{"uri": resourceURIPrefix + "system/graph"}, "")
	if capped.Error == nil {
		t.Fatal("resource exceeded response cap")
	}
}
