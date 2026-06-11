package config

import (
	"os"
	"testing"
)

// clearGraphRAGBoundsEnv unsets the GraphRAG memory-bound env vars so Load()
// starts from a deterministic "operator set nothing" baseline. Mirrors
// clearSQLiteEnv (driver_defaults_test.go).
func clearGraphRAGBoundsEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GRAPHRAG_TRACE_TTL",
		"GRAPHRAG_MAX_SPANS_PER_TENANT",
		"GRAPHRAG_TENANT_IDLE_TTL",
	} {
		if _, ok := os.LookupEnv(k); ok {
			old := os.Getenv(k)
			t.Setenv(k, old) // record original for revert
			if err := os.Unsetenv(k); err != nil {
				t.Fatalf("unset %s: %v", k, err)
			}
		}
	}
}

// TestLoad_GraphRAGMemoryBounds_Defaults proves the new GraphRAG memory-bound
// knobs land with their documented Postgres-path defaults: 1h trace TTL,
// 500k spans per tenant, 24h tenant idle TTL.
func TestLoad_GraphRAGMemoryBounds_Defaults(t *testing.T) {
	clearGraphRAGBoundsEnv(t)
	t.Setenv("DB_DRIVER", "postgres") // avoid the SQLite 30m TTL override

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GraphRAGTraceTTL != "1h" {
		t.Errorf("GraphRAGTraceTTL = %q, want \"1h\"", cfg.GraphRAGTraceTTL)
	}
	if cfg.GraphRAGMaxSpansPerTenant != 500000 {
		t.Errorf("GraphRAGMaxSpansPerTenant = %d, want 500000", cfg.GraphRAGMaxSpansPerTenant)
	}
	if cfg.GraphRAGTenantIdleTTL != "24h" {
		t.Errorf("GraphRAGTenantIdleTTL = %q, want \"24h\"", cfg.GraphRAGTenantIdleTTL)
	}
}

// TestLoad_GraphRAGMemoryBounds_EnvOverride proves operator-set values are
// honoured verbatim.
func TestLoad_GraphRAGMemoryBounds_EnvOverride(t *testing.T) {
	clearGraphRAGBoundsEnv(t)
	t.Setenv("DB_DRIVER", "sqlite")
	t.Setenv("GRAPHRAG_TRACE_TTL", "15m")
	t.Setenv("GRAPHRAG_MAX_SPANS_PER_TENANT", "1000")
	t.Setenv("GRAPHRAG_TENANT_IDLE_TTL", "1h")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GraphRAGTraceTTL != "15m" {
		t.Errorf("GraphRAGTraceTTL = %q, want \"15m\" (SQLite override must not clobber explicit env)", cfg.GraphRAGTraceTTL)
	}
	if cfg.GraphRAGMaxSpansPerTenant != 1000 {
		t.Errorf("GraphRAGMaxSpansPerTenant = %d, want 1000", cfg.GraphRAGMaxSpansPerTenant)
	}
	if cfg.GraphRAGTenantIdleTTL != "1h" {
		t.Errorf("GraphRAGTenantIdleTTL = %q, want \"1h\"", cfg.GraphRAGTenantIdleTTL)
	}
}
