package storage

import (
	"context"
	"strings"
	"unicode"
)

// MaxTenantIDLength caps the length of an accepted tenant ID. Tenant IDs are
// stored in a VARCHAR(64) column on every domain row plus propagate into
// structured logs and Prometheus labels. The cap is a defense in depth against
// silent VARCHAR truncation at insert time and unbounded label cardinality
// from a hostile or misconfigured client.
const MaxTenantIDLength = 128

// SanitizeTenantID validates and normalizes a tenant ID supplied by an HTTP
// header, gRPC metadata key, or OTLP resource attribute. It returns the empty
// string for any value the caller should reject (and substitute with their
// configured default), so the rejection contract is uniform across transports.
//
// Rejection criteria:
//   - empty after TrimSpace
//   - length exceeds MaxTenantIDLength after trim
//   - contains a Unicode control character (\n, \r, \t, NUL, escape codes)
//
// On the happy path it returns the trimmed value verbatim — no case folding,
// no allowlist, since legitimate tenant IDs may be UUIDs, slugs, or
// organisation names in non-ASCII scripts.
func SanitizeTenantID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > MaxTenantIDLength {
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return s
}

// tenantCtxKey is the private context key used to carry the resolved tenant ID
// through an HTTP (or gRPC) request down into the repository layer.
type tenantCtxKey struct{}

// WithTenantContext returns a copy of ctx that carries tenant as its tenant ID.
// Empty strings are coerced to DefaultTenantID so downstream queries always have
// a non-empty value to filter on.
func WithTenantContext(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		tenant = DefaultTenantID
	}
	return context.WithValue(ctx, tenantCtxKey{}, tenant)
}

// TenantFromContext returns the tenant ID stashed on ctx by TenantMiddleware
// (or WithTenantContext). When the context carries no tenant, or a nil context
// is passed, it returns DefaultTenantID so single-tenant installs behave as
// before this feature landed.
func TenantFromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultTenantID
	}
	v := ctx.Value(tenantCtxKey{})
	if v == nil {
		return DefaultTenantID
	}
	t, ok := v.(string)
	if !ok || t == "" {
		return DefaultTenantID
	}
	return t
}

// HasTenantContext reports whether ctx carries a tenant value set by
// WithTenantContext. It lets callers (e.g. OTLP ingest) distinguish
// "no tenant set at all" (fall through to other sources like gRPC metadata)
// from "explicitly set to default tenant" — a distinction TenantFromContext
// erases by returning DefaultTenantID for both cases.
func HasTenantContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(tenantCtxKey{})
	if v == nil {
		return false
	}
	s, ok := v.(string)
	return ok && s != ""
}
