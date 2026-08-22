package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// conflictRecorder captures the tenant-conflict metric hook for one test.
type conflictRecorder struct{ calls [][2]string }

func (c *conflictRecorder) install(t *testing.T) {
	t.Helper()
	prev := authn.ConflictHook
	t.Cleanup(func() { authn.ConflictHook = prev })
	authn.ConflictHook = func(surface, reason string) {
		c.calls = append(c.calls, [2]string{surface, reason})
	}
}

func tenantKeyAuth(t *testing.T, operatorKey string, entries map[string]string) *authn.Authenticator {
	t.Helper()
	var store *authn.KeyStore
	if len(entries) > 0 {
		var err error
		store, err = authn.NewKeyStoreFromMap(entries)
		if err != nil {
			t.Fatalf("NewKeyStoreFromMap: %v", err)
		}
	}
	return authn.NewAuthenticator(operatorKey, store, false)
}

// tenantEcho reports the tenant the request context carried into the handler.
func tenantEcho(got *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = storage.TenantFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

// A tenant key binds the request: a contradicting X-Tenant-ID is ignored AND
// counted. This is the cross-tenant read vector from #194 blocker 7.
func TestAuthGate_TenantKeyIgnoresClientTenantHeader(t *testing.T) {
	var rec conflictRecorder
	rec.install(t)

	var got string
	h := AuthGate(AuthGateOptions{Auth: tenantKeyAuth(t, "", map[string]string{"acme-key": "acme"}), MCPPath: "/mcp"}, tenantEcho(&got))

	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Authorization", "Bearer acme-key")
	req.Header.Set(TenantHeader, "victim")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got != "acme" {
		t.Errorf("tenant = %q, want acme (key binding must win)", got)
	}
	if len(rec.calls) != 1 || rec.calls[0] != [2]string{"http", "header"} {
		t.Errorf("conflict metric = %v, want one http/header record", rec.calls)
	}
}

// A matching X-Tenant-ID is not a conflict: the client is simply agreeing.
func TestAuthGate_TenantKeyAgreeingHeaderIsNotCounted(t *testing.T) {
	var rec conflictRecorder
	rec.install(t)

	var got string
	h := AuthGate(AuthGateOptions{Auth: tenantKeyAuth(t, "", map[string]string{"acme-key": "acme"})}, tenantEcho(&got))
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Authorization", "Bearer acme-key")
	req.Header.Set(TenantHeader, "acme")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(rec.calls) != 0 {
		t.Errorf("agreeing header counted as conflict: %v", rec.calls)
	}
}

func TestAuthGate_UnknownAndMissingCredentials(t *testing.T) {
	auth := tenantKeyAuth(t, "", map[string]string{"acme-key": "acme"})
	var reasons []string
	prev := AuthFailureHook
	t.Cleanup(func() { AuthFailureHook = prev })
	AuthFailureHook = func(reason string) { reasons = append(reasons, reason) }

	h := AuthGate(AuthGateOptions{Auth: auth, MCPPath: "/mcp"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		path   string
		header string
		code   int
	}{
		{"unknown key", "/api/logs", "Bearer nope", http.StatusUnauthorized},
		{"missing header", "/api/logs", "", http.StatusUnauthorized},
		{"bad scheme", "/api/logs", "Basic acme-key", http.StatusUnauthorized},
		{"otlp http protected", "/v1/traces", "", http.StatusUnauthorized},
		{"mcp protected", "/mcp", "", http.StatusUnauthorized},
		{"public probe", "/live", "", http.StatusOK},
		{"websocket handled elsewhere", "/ws/events", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.code {
				t.Fatalf("%s: want %d, got %d", tc.path, tc.code, w.Code)
			}
		})
	}
	want := []string{"bad_key", "missing_header", "bad_scheme", "missing_header", "missing_header"}
	if len(reasons) != len(want) {
		t.Fatalf("auth-failure reasons = %v, want %v", reasons, want)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Errorf("reason[%d] = %q, want %q", i, reasons[i], want[i])
		}
	}
}

// The shared API_KEY keeps its exact pre-existing behaviour: it authenticates
// and nothing more, so tenant resolution still follows the X-Tenant-ID header.
// Byte-for-byte identical responses to the legacy APIKeyGate.
func TestAuthGate_OperatorKeyMatchesLegacyAPIKeyGate(t *testing.T) {
	inner := func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
	}
	legacy := APIKeyGate("secret", "/mcp", inner())
	gate := AuthGate(AuthGateOptions{Auth: tenantKeyAuth(t, "secret", nil), MCPPath: "/mcp"}, inner())

	cases := []struct{ path, header string }{
		{"/api/logs", "Bearer secret"},
		{"/api/logs", "Bearer wrong"},
		{"/api/logs", ""},
		{"/api/logs", "Basic secret"},
		{"/v1/traces", "Bearer secret"},
		{"/mcp", ""},
		{"/live", ""},
		{"/ws/events", ""},
		{"/assets/app.js", ""},
	}
	for _, tc := range cases {
		newReq := func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			return r
		}
		lw, gw := httptest.NewRecorder(), httptest.NewRecorder()
		legacy.ServeHTTP(lw, newReq())
		gate.ServeHTTP(gw, newReq())
		if lw.Code != gw.Code || lw.Body.String() != gw.Body.String() {
			t.Errorf("%s %q: legacy=(%d,%q) gate=(%d,%q)", tc.path, tc.header, lw.Code, lw.Body.String(), gw.Code, gw.Body.String())
		}
	}
}

// Operator credentials leave tenant selection to TenantMiddleware, so the
// X-Tenant-ID header still decides — the single-tenant install is untouched.
func TestAuthGate_OperatorKeyDoesNotBindTenant(t *testing.T) {
	var rec conflictRecorder
	rec.install(t)

	var got string
	h := AuthGate(AuthGateOptions{Auth: tenantKeyAuth(t, "secret", map[string]string{"acme-key": "acme"})},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if storage.HasTenantContext(r.Context()) {
				got = storage.TenantFromContext(r.Context())
			}
			w.WriteHeader(http.StatusOK)
		}))
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(TenantHeader, "beta")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != "" {
		t.Errorf("operator request pinned tenant %q; header precedence must stay with TenantMiddleware", got)
	}
	if len(rec.calls) != 0 {
		t.Errorf("operator request counted a conflict: %v", rec.calls)
	}
}

// Default deployment: no credential source at all is a pass-through, so
// today's development behaviour survives byte-for-byte.
func TestAuthGate_DisabledIsPassthrough(t *testing.T) {
	var got string
	h := AuthGate(AuthGateOptions{Auth: authn.NewAuthenticator("", nil, false)}, tenantEcho(&got))
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

// AUTH_TRUST_EXTERNAL: the dedicated header is an identity, X-Tenant-ID is not.
func TestAuthGate_ExternalIdentityHeader(t *testing.T) {
	auth := authn.NewAuthenticator("", nil, true)
	var got string
	h := AuthGate(AuthGateOptions{Auth: auth}, tenantEcho(&got))

	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set(DefaultExternalTenantHeader, "acme")
	req.Header.Set(TenantHeader, "victim")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || got != "acme" {
		t.Fatalf("injected identity: code=%d tenant=%q, want 200/acme", w.Code, got)
	}

	// Without the dedicated header the request is unauthenticated: the
	// client-controlled tenant header is never an identity.
	req = httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set(TenantHeader, "acme")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("X-Tenant-ID alone: want 401, got %d", w.Code)
	}

	// A presented-but-invalid bearer credential is a 401 even under external
	// trust — a bad key must never silently downgrade to the header identity.
	auth2 := authn.NewAuthenticator("secret", nil, true)
	h2 := AuthGate(AuthGateOptions{Auth: auth2}, tenantEcho(&got))
	req = httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set(DefaultExternalTenantHeader, "acme")
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad key + injected header: want 401, got %d", w.Code)
	}
}

// The key file itself is loaded by internal/authn; this only pins the wiring
// contract main.go relies on (JSON by extension, 0600, digest-only).
func TestAuthGate_LoadedFromKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(path, []byte(`{"acme-key":"acme","beta-key":"beta"}`), 0o600); err != nil {
		t.Fatalf("write keys: %v", err)
	}
	store, err := authn.LoadKeyStore(path)
	if err != nil {
		t.Fatalf("LoadKeyStore: %v", err)
	}
	var got string
	h := AuthGate(AuthGateOptions{Auth: authn.NewAuthenticator("", store, false)}, tenantEcho(&got))
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Authorization", "Bearer beta-key")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "beta" {
		t.Fatalf("tenant = %q, want beta", got)
	}
}
