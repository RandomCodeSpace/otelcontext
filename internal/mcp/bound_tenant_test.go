package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

// mcpConflictRecorder captures authn.RecordConflict calls for one test.
type mcpConflictRecorder struct{ calls [][2]string }

func (c *mcpConflictRecorder) install(t *testing.T) {
	t.Helper()
	prev := authn.ConflictHook
	t.Cleanup(func() { authn.ConflictHook = prev })
	authn.ConflictHook = func(surface, reason string) {
		c.calls = append(c.calls, [2]string{surface, reason})
	}
}

// boundRequest builds an MCP request carrying a bound tenant principal, the
// way api.AuthGate stashes it before the MCP handler runs, plus an optional
// contradicting X-Tenant-ID header.
func boundRequest(method, bound, header string, body []byte) *http.Request {
	req := httptest.NewRequest(method, "/mcp", bytes.NewReader(body))
	if header != "" {
		req.Header.Set(mcpTenantHeader, header)
	}
	if bound != "" {
		p := authn.Principal{Kind: authn.KindTenant, Tenant: bound}
		req = req.WithContext(authn.WithPrincipal(req.Context(), p))
	}
	return req
}

func callServiceMapText(t *testing.T, srv *Server, req *http.Request) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResp(t, rec.Body.Bytes())
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %v", resp.Error)
	}
	var got ToolCallResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var sb strings.Builder
	for _, c := range got.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// TestMCP_BoundTenant_POSTIgnoresHeader proves a tools/call from a bound
// principal runs under the bound tenant even when X-Tenant-ID names another
// one, and that the ignored assertion is counted on surface "mcp".
func TestMCP_BoundTenant_POSTIgnoresHeader(t *testing.T) {
	rec := &mcpConflictRecorder{}
	rec.install(t)

	srv := minimalServer(t)
	srv.SetCallLimit(0)
	srv.SetCacheTTL(5 * time.Second)
	srv.cache.Set("acme", "get_service_map", nil, ToolCallResult{Content: []ContentItem{{Type: "text", Text: "cached-acme"}}})
	srv.cache.Set("beta", "get_service_map", nil, ToolCallResult{Content: []ContentItem{{Type: "text", Text: "cached-beta"}}})

	body := jsonRPCCallToolBody(t, "get_service_map", nil)
	if got := callServiceMapText(t, srv, boundRequest(http.MethodPost, "acme", "beta", body)); got != "cached-acme" {
		t.Fatalf("bound principal did not run under its tenant: got %q", got)
	}
	if len(rec.calls) != 1 || rec.calls[0] != [2]string{"mcp", "header"} {
		t.Fatalf("conflict calls = %v, want [[mcp header]]", rec.calls)
	}

	// A header that agrees with the binding is not a conflict.
	rec.calls = nil
	if got := callServiceMapText(t, srv, boundRequest(http.MethodPost, "acme", "acme", body)); got != "cached-acme" {
		t.Fatalf("agreeing header changed the tenant: got %q", got)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("agreeing header counted as conflict: %v", rec.calls)
	}
}

// TestMCP_BoundTenant_CacheIsolated proves two bound principals asking for
// the same cacheable tool get their own cache entries.
func TestMCP_BoundTenant_CacheIsolated(t *testing.T) {
	srv := minimalServer(t)
	srv.SetCallLimit(0)
	srv.SetCacheTTL(5 * time.Second)
	srv.cache.Set("tenant-a", "get_service_map", nil, ToolCallResult{Content: []ContentItem{{Type: "text", Text: "cached-a"}}})

	body := jsonRPCCallToolBody(t, "get_service_map", nil)
	if got := callServiceMapText(t, srv, boundRequest(http.MethodPost, "tenant-a", "", body)); got != "cached-a" {
		t.Fatalf("tenant-a: got %q, want cached-a", got)
	}
	if got := callServiceMapText(t, srv, boundRequest(http.MethodPost, "tenant-b", "", body)); got == "cached-a" {
		t.Fatalf("tenant-b received tenant-a's cached result")
	}
	if hits := srv.Stats().CacheHits; hits != 1 {
		t.Fatalf("CacheHits = %d, want 1", hits)
	}
}

// TestMCP_UnauthenticatedHeaderSelectsTenant is the regression guard: with
// no principal on the context the header still picks the tenant.
func TestMCP_UnauthenticatedHeaderSelectsTenant(t *testing.T) {
	rec := &mcpConflictRecorder{}
	rec.install(t)

	srv := minimalServer(t)
	srv.SetCallLimit(0)
	srv.SetCacheTTL(5 * time.Second)
	srv.cache.Set("beta", "get_service_map", nil, ToolCallResult{Content: []ContentItem{{Type: "text", Text: "cached-beta"}}})

	body := jsonRPCCallToolBody(t, "get_service_map", nil)
	if got := callServiceMapText(t, srv, boundRequest(http.MethodPost, "", "beta", body)); got != "cached-beta" {
		t.Fatalf("header tenant not honoured without a principal: got %q", got)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("unauthenticated request counted a conflict: %v", rec.calls)
	}

	// An operator principal is not bound and keeps the header precedence.
	req := boundRequest(http.MethodPost, "", "beta", body)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{Kind: authn.KindOperator}))
	if got := callServiceMapText(t, srv, req); got != "cached-beta" {
		t.Fatalf("operator header tenant not honoured: got %q", got)
	}
}

// tenantRecordingProvider records the tenant on the context it was called
// with, so the SSE test can observe which tenant the stream runs under.
type tenantRecordingProvider struct {
	fakeMCPTopologyProvider
	seen []string
}

func (p *tenantRecordingProvider) Identity(ctx context.Context) topology.Identity {
	p.seen = append(p.seen, storage.TenantFromContext(ctx))
	return p.fakeMCPTopologyProvider.Identity(ctx)
}

func (p *tenantRecordingProvider) Snapshot(ctx context.Context, q topology.Query) (topology.Snapshot, error) {
	p.seen = append(p.seen, storage.TenantFromContext(ctx))
	return p.fakeMCPTopologyProvider.Snapshot(ctx, q)
}

// TestMCP_BoundTenant_SSEIgnoresHeader proves the SSE stream's tenant
// context is the bound one, not the contradicting header.
func TestMCP_BoundTenant_SSEIgnoresHeader(t *testing.T) {
	rec := &mcpConflictRecorder{}
	rec.install(t)

	provider := &tenantRecordingProvider{fakeMCPTopologyProvider: fakeMCPTopologyProvider{
		epoch:    "boot-a",
		snapshot: topology.Snapshot{Nodes: []topology.Node{{Name: "gateway"}}, Meta: topology.Metadata{Coverage: "full"}},
	}}
	provider.revision.Store(1)
	srv := New("default", nil, nil, provider)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := boundRequest(http.MethodGet, "acme", "beta", nil).WithContext(
		authn.WithPrincipal(ctx, authn.Principal{Kind: authn.KindTenant, Tenant: "acme"}))
	recorder := httptest.NewRecorder()
	srv.handleSSE(recorder, req)

	if len(provider.seen) == 0 {
		t.Fatalf("provider never called: %s", recorder.Body.String())
	}
	for _, tenant := range provider.seen {
		if tenant != "acme" {
			t.Fatalf("SSE ran under tenant %q, want acme (seen %v)", tenant, provider.seen)
		}
	}
	if len(rec.calls) != 1 || rec.calls[0] != [2]string{"mcp", "header"} {
		t.Fatalf("conflict calls = %v, want [[mcp header]]", rec.calls)
	}
}
