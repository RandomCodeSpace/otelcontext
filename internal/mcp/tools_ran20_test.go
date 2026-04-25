package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

// TestFindSimilarLogs_TenantIsolation is the RAN-20 acceptance bar for the MCP
// surface. Two tenants with unique marker strings in their log bodies query
// find_similar_logs; each tenant's response must never contain the other's
// markers.
func TestFindSimilarLogs_TenantIsolation(t *testing.T) {
	idx := vectordb.New(1_000)
	idx.Add(101, "acme", "checkout", "ERROR", "payment gateway timeout acme-secret-charge-id-abc")
	idx.Add(102, "acme", "checkout", "ERROR", "payment gateway refused acme-only-marker-xyz")
	idx.Add(201, "globex", "auth", "ERROR", "payment gateway token expired globex-secret-session-123")
	idx.Add(202, "globex", "auth", "ERROR", "payment gateway 500 internal globex-only-marker-qqq")

	srv := &Server{vectorIdx: idx, defaultTenant: storage.DefaultTenantID}
	args := map[string]any{"query": "payment gateway", "limit": float64(50)}

	// Acme
	acmeRes := srv.toolFindSimilarLogs(storage.WithTenantContext(context.Background(), "acme"), args)
	if acmeRes.IsError {
		t.Fatalf("acme call errored: %+v", acmeRes)
	}
	acmeBody := concatContent(acmeRes.Content)
	for _, forbidden := range []string{"globex-secret-session-123", "globex-only-marker-qqq", `"LogID": 201`, `"LogID": 202`} {
		if strings.Contains(acmeBody, forbidden) {
			t.Fatalf("acme leaked globex content %q in body:\n%s", forbidden, acmeBody)
		}
	}
	if !strings.Contains(acmeBody, "acme-secret-charge-id-abc") && !strings.Contains(acmeBody, "acme-only-marker-xyz") {
		t.Fatalf("acme did not receive its own rows:\n%s", acmeBody)
	}

	// Globex
	gRes := srv.toolFindSimilarLogs(storage.WithTenantContext(context.Background(), "globex"), args)
	if gRes.IsError {
		t.Fatalf("globex call errored: %+v", gRes)
	}
	gBody := concatContent(gRes.Content)
	for _, forbidden := range []string{"acme-secret-charge-id-abc", "acme-only-marker-xyz", `"LogID": 101`, `"LogID": 102`} {
		if strings.Contains(gBody, forbidden) {
			t.Fatalf("globex leaked acme content %q in body:\n%s", forbidden, gBody)
		}
	}
}

// TestFindSimilarLogs_NoTenantFallsBackToDefault proves that a context with no
// tenant value is coerced to the server default — it must NOT bleed into
// another tenant's rows.
func TestFindSimilarLogs_NoTenantFallsBackToDefault(t *testing.T) {
	idx := vectordb.New(100)
	idx.Add(1, "acme", "svc", "ERROR", "acme secret body only")

	srv := &Server{vectorIdx: idx, defaultTenant: storage.DefaultTenantID}
	args := map[string]any{"query": "secret body"}

	res := srv.toolFindSimilarLogs(context.Background(), args)
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	if strings.Contains(concatContent(res.Content), "acme secret body only") {
		t.Fatalf("no-tenant call leaked acme content:\n%s", concatContent(res.Content))
	}
}

func concatContent(items []ContentItem) string {
	var b strings.Builder
	for _, c := range items {
		b.WriteString(c.Text)
	}
	return b.String()
}
