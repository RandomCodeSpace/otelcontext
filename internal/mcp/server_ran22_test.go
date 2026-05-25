package mcp

import (
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// TestNew_DefaultTenant_FromConstructor is the RAN-22 regression bar at the
// type level: the configured default tenant is a required leading parameter
// to New, so production startup wiring (main.go) cannot drop it without a
// compile error. Empty input falls back to storage.DefaultTenantID; a
// non-empty value is preserved verbatim.
//
// End-to-end coverage that the configured default actually flows through the
// HTTP transport into the tenant-scoped tool path is provided by
// tenant_isolation_test.go::TestMCP_TenantIsolation_AllGraphRAGTools (the
// no-header caller).
func TestNew_DefaultTenant_FromConstructor(t *testing.T) {
	t.Run("empty falls back to storage.DefaultTenantID", func(t *testing.T) {
		srv := New("", nil, nil, nil)
		if srv.defaultTenant != storage.DefaultTenantID {
			t.Fatalf(`New("") defaultTenant = %q, want %q`, srv.defaultTenant, storage.DefaultTenantID)
		}
	})
	t.Run("non-empty value is preserved", func(t *testing.T) {
		srv := New("acme", nil, nil, nil)
		if srv.defaultTenant != "acme" {
			t.Fatalf(`New("acme") defaultTenant = %q, want "acme"`, srv.defaultTenant)
		}
	})
	t.Run("SetDefaultTenant runtime override still works", func(t *testing.T) {
		srv := New("acme", nil, nil, nil)
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
