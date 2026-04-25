package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

// TestSetDefaultTenant_PropagatesToHTTPTransport is the RAN-22 acceptance bar:
// after startup wiring calls SetDefaultTenant(cfg.DefaultTenant), a header-less
// MCP tools/call must resolve to the configured tenant rather than the
// hardcoded storage.DefaultTenantID. Also asserts the no-op semantics for
// SetDefaultTenant("") so we don't regress the documented contract.
func TestSetDefaultTenant_PropagatesToHTTPTransport(t *testing.T) {
	idx := vectordb.New(100)
	idx.Add(1, "acme", "checkout", "ERROR", "payment gateway timeout acme-marker-xyz")
	idx.Add(2, "globex", "auth", "ERROR", "payment gateway 500 globex-marker-qqq")
	idx.Add(3, "default", "svc", "ERROR", "payment gateway refused default-marker-aaa")

	srv := New(nil, nil, nil, idx)
	if srv.defaultTenant != storage.DefaultTenantID {
		t.Fatalf("constructor default = %q, want %q", srv.defaultTenant, storage.DefaultTenantID)
	}

	// Empty argument is a no-op — preserves prior value.
	srv.SetDefaultTenant("")
	if srv.defaultTenant != storage.DefaultTenantID {
		t.Fatalf(`SetDefaultTenant("") clobbered field to %q`, srv.defaultTenant)
	}

	body := mustMarshalJSONRPC(t, "find_similar_logs", map[string]any{
		"query": "payment gateway",
		"limit": float64(50),
	})

	// Step 1: configure non-default tenant; no header → should be acme-scoped.
	srv.SetDefaultTenant("acme")
	if srv.defaultTenant != "acme" {
		t.Fatalf(`SetDefaultTenant("acme") = %q`, srv.defaultTenant)
	}
	resp1 := callNoHeader(t, srv, body)
	mustContain(t, resp1, "acme-marker-xyz")
	mustNotContain(t, resp1, "globex-marker-qqq", "default-marker-aaa")

	// Step 2: change configured tenant; same header-less call must follow.
	srv.SetDefaultTenant("globex")
	resp2 := callNoHeader(t, srv, body)
	mustContain(t, resp2, "globex-marker-qqq")
	mustNotContain(t, resp2, "acme-marker-xyz", "default-marker-aaa")

	// Step 3: explicit X-Tenant-ID header must still win over the configured
	// default — proves the fix did not invert tenant precedence.
	resp3 := callWithHeader(t, srv, body, "acme")
	mustContain(t, resp3, "acme-marker-xyz")
	mustNotContain(t, resp3, "globex-marker-qqq", "default-marker-aaa")
}

func mustMarshalJSONRPC(t *testing.T, tool string, args map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func callNoHeader(t *testing.T, srv *Server, body []byte) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func callWithHeader(t *testing.T, srv *Server, body []byte, tenant string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenant)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("response missing expected marker %q:\n%s", want, body)
	}
}

func mustNotContain(t *testing.T, body string, forbidden ...string) {
	t.Helper()
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Fatalf("response leaked forbidden marker %q:\n%s", f, body)
		}
	}
}
