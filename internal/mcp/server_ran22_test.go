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

// TestNew_DefaultTenant_FromConstructor is the RAN-22 regression bar at the
// type level: the configured default tenant is a required leading parameter
// to New, so production startup wiring (main.go) cannot drop it without a
// compile error. Empty input falls back to storage.DefaultTenantID; a
// non-empty value is preserved verbatim.
func TestNew_DefaultTenant_FromConstructor(t *testing.T) {
	t.Run("empty falls back to storage.DefaultTenantID", func(t *testing.T) {
		srv := New("", nil, nil, nil, vectordb.New(1))
		if srv.defaultTenant != storage.DefaultTenantID {
			t.Fatalf(`New("") defaultTenant = %q, want %q`, srv.defaultTenant, storage.DefaultTenantID)
		}
	})
	t.Run("non-empty value is preserved", func(t *testing.T) {
		srv := New("acme", nil, nil, nil, vectordb.New(1))
		if srv.defaultTenant != "acme" {
			t.Fatalf(`New("acme") defaultTenant = %q, want "acme"`, srv.defaultTenant)
		}
	})
	t.Run("SetDefaultTenant runtime override still works", func(t *testing.T) {
		srv := New("acme", nil, nil, nil, vectordb.New(1))
		srv.SetDefaultTenant("globex")
		if srv.defaultTenant != "globex" {
			t.Fatalf(`SetDefaultTenant("globex") defaultTenant = %q, want "globex"`, srv.defaultTenant)
		}
		// Empty argument is a no-op so optional config doesn't clobber.
		srv.SetDefaultTenant("")
		if srv.defaultTenant != "globex" {
			t.Fatalf(`SetDefaultTenant("") clobbered field to %q`, srv.defaultTenant)
		}
	})
}

// TestNew_DefaultTenant_FlowsThroughHTTPTransport proves that the constructor-
// supplied tenant is the actual fallback used by the JSON-RPC HTTP handler
// when no X-Tenant-ID header is present, and that an explicit header still
// wins over the default. This locks in the end-to-end behavior the RAN-22
// fix delivers: a deployment with DEFAULT_TENANT=acme returns acme-scoped
// data from header-less MCP tool calls.
func TestNew_DefaultTenant_FlowsThroughHTTPTransport(t *testing.T) {
	idx := vectordb.New(100)
	idx.Add(1, "acme", "checkout", "ERROR", "payment gateway timeout acme-marker-xyz")
	idx.Add(2, "globex", "auth", "ERROR", "payment gateway 500 globex-marker-qqq")
	idx.Add(3, "default", "svc", "ERROR", "payment gateway refused default-marker-aaa")

	body := mustMarshalJSONRPC(t, "find_similar_logs", map[string]any{
		"query": "payment gateway",
		"limit": float64(50),
	})

	srv := New("acme", nil, nil, nil, idx)

	// Header-less tools/call must scope to the constructor-provided default.
	resp1 := callNoHeader(t, srv, body)
	mustContain(t, resp1, "acme-marker-xyz")
	mustNotContain(t, resp1, "globex-marker-qqq", "default-marker-aaa")

	// Explicit X-Tenant-ID header beats the configured default — precedence
	// invariant is preserved.
	resp2 := callWithHeader(t, srv, body, "globex")
	mustContain(t, resp2, "globex-marker-qqq")
	mustNotContain(t, resp2, "acme-marker-xyz", "default-marker-aaa")

	// SetDefaultTenant runtime override flows to the same transport path so
	// future runtime-config-reload paths behave correctly.
	srv.SetDefaultTenant("globex")
	resp3 := callNoHeader(t, srv, body)
	mustContain(t, resp3, "globex-marker-qqq")
	mustNotContain(t, resp3, "acme-marker-xyz", "default-marker-aaa")
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
