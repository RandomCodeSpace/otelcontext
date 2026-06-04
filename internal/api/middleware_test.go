package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// okHandler returns 200 — used as the inner handler for middleware tests.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestRequireAPIKey_ValidKey_Passes(t *testing.T) {
	h := RequireAPIKey("s3cret", okHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestRequireAPIKey_BadKey_401(t *testing.T) {
	h := RequireAPIKey("s3cret", okHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRequireAPIKey_NoKey_401(t *testing.T) {
	h := RequireAPIKey("s3cret", okHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	// no Authorization header
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRequireAPIKey_Disabled_Passthrough(t *testing.T) {
	h := RequireAPIKey("", okHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	// no Authorization — should still pass because auth is disabled
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 pass-through, got %d", rec.Code)
	}
}

func TestAPIKeyGate_PublicPathSkipsAuth(t *testing.T) {
	h := APIKeyGate("s3cret", "/mcp", okHandler())
	for _, path := range []string{"/", "/live", "/ready", "/ws/events", "/metrics/prometheus", "/assets/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("public path %q should skip auth, got %d", path, rec.Code)
		}
	}
}

func TestAPIKeyGate_ProtectedPathsRequireKey(t *testing.T) {
	h := APIKeyGate("s3cret", "/mcp", okHandler())
	for _, path := range []string{"/api/logs", "/v1/traces", "/mcp", "/mcp/tools"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("protected path %q should require key, got %d", path, rec.Code)
		}
	}
}

// tenantCapture is a handler that records the tenant stashed on the request
// context by TenantMiddleware so the test can assert on it.
type tenantCapture struct{ got string }

func (tc *tenantCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc.got = storage.TenantFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestTenantMiddleware_HeaderPropagated(t *testing.T) {
	cfg := &config.Config{DefaultTenant: "default"}
	tc := &tenantCapture{}
	h := TenantMiddleware(cfg)(tc.handler())

	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set(TenantHeader, "acme")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if tc.got != "acme" {
		t.Fatalf("tenant header not propagated: want %q got %q", "acme", tc.got)
	}
}

func TestTenantMiddleware_MissingHeaderUsesDefault(t *testing.T) {
	cfg := &config.Config{DefaultTenant: "house"}
	tc := &tenantCapture{}
	h := TenantMiddleware(cfg)(tc.handler())

	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	// no X-Tenant-ID header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if tc.got != "house" {
		t.Fatalf("missing header should use cfg default: want %q got %q", "house", tc.got)
	}
}

// TestTenantMiddleware_DoesNotOverwritePinnedTenant verifies that when
// TenantKeyAuth.Middleware has already pinned a tenant onto the context (the
// per-tenant API-key path), the subsequent TenantMiddleware pass-through does
// NOT overwrite it with the client-supplied X-Tenant-ID header.
//
// This is the regression test for the middleware-ordering bypass:
//
//	TenantKeyAuth.Middleware(auth "alpha-key" → pins "alpha")
//	  → TenantMiddleware(reads X-Tenant-ID: "beta" → must NOT overwrite)
//	    → handler (must see "alpha")
func TestTenantMiddleware_DoesNotOverwritePinnedTenant(t *testing.T) {
	// Build per-tenant key auth: key "alpha-key" → tenant "alpha".
	auth := NewTenantKeyAuth(map[string]string{"alpha-key": "alpha"})

	cfg := &config.Config{DefaultTenant: "default"}
	tc := &tenantCapture{}

	// Compose: TenantKeyAuth wraps TenantMiddleware wraps handler.
	h := auth.Middleware("/mcp", TenantMiddleware(cfg)(tc.handler()))

	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Authorization", "Bearer alpha-key")
	req.Header.Set(TenantHeader, "beta") // attacker's cross-tenant attempt

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if tc.got != "alpha" {
		t.Errorf("TenantMiddleware overwrote pinned tenant: got %q, want %q", tc.got, "alpha")
	}
}

// Non-/api/* paths must pass through without tenant resolution — and so should
// report the default (no ctx value).
func TestTenantMiddleware_NonAPIPath_Passthrough(t *testing.T) {
	cfg := &config.Config{DefaultTenant: "ignored"}
	tc := &tenantCapture{}
	h := TenantMiddleware(cfg)(tc.handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
	req.Header.Set(TenantHeader, "acme") // must be ignored
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if tc.got != storage.DefaultTenantID {
		t.Fatalf("non-API path should not resolve tenant: want %q got %q", storage.DefaultTenantID, tc.got)
	}
}
