package api

import (
	"net/http"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)


// TenantHeader is the canonical HTTP header carrying the tenant ID on
// read-side (query) requests. Ingest paths resolve tenant separately via gRPC
// metadata / OTLP resource attributes and do not go through this middleware.
const TenantHeader = "X-Tenant-ID"

// TenantMiddleware extracts the tenant ID from the X-Tenant-ID header (falling
// back to cfg.DefaultTenant — or "default" when cfg is nil or empty) and
// stashes it on the request context via storage.WithTenantContext so repository
// reads can scope their WHERE clause with storage.TenantFromContext.
//
// The middleware is path-aware: only requests whose path begins with "/api/"
// are tenant-scoped. OTLP write endpoints ("/v1/..."), health probes
// ("/live", "/ready"), Prometheus scrape ("/metrics/..."), MCP, WebSocket and
// UI assets pass through untouched — these either resolve tenant separately
// (OTLP) or are tenant-agnostic/privileged.
func TenantMiddleware(cfg *config.Config) func(http.Handler) http.Handler {
	defaultTenant := storage.DefaultTenantID
	if cfg != nil && cfg.DefaultTenant != "" {
		defaultTenant = cfg.DefaultTenant
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !tenantScopedPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			// SanitizeTenantID returns "" for empty / over-length / control-char
			// values so they fall through to the configured default — see
			// storage.SanitizeTenantID. Hostile or misconfigured clients cannot
			// inject newlines into structured logs or overflow VARCHAR(64).
			tenant := storage.SanitizeTenantID(r.Header.Get(TenantHeader))
			if tenant == "" {
				tenant = defaultTenant
			}
			ctx := storage.WithTenantContext(r.Context(), tenant)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// tenantScopedPath reports whether an inbound HTTP path should have tenant
// resolution applied. Currently only /api/* is tenant-scoped on the read side.
func tenantScopedPath(path string) bool {
	return strings.HasPrefix(path, "/api/")
}
