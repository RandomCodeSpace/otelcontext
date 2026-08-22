package telemetry

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// AuthnMetrics covers the authenticated-tenant-identity surfaces (HTTP,
// WebSocket, gRPC). It lives apart from Metrics because it is wired from
// package-level hooks in authn/api/ingest rather than passed down the call
// graph, and because a duplicate registration must be impossible even when a
// test constructs the platform twice.
type AuthnMetrics struct {
	// TenantConflictsTotal counts client-asserted tenants that an
	// authenticated binding overrode. Labels: surface (http|ws|grpc), reason
	// (header|query|metadata|resource_attribute). A non-zero rate means a
	// client is sending a tenant it is not entitled to — misconfiguration at
	// best, a probe at worst.
	TenantConflictsTotal *prometheus.CounterVec

	// GRPCAuthFailuresTotal counts rejected gRPC calls by reason
	// (missing_header|bad_scheme|bad_key).
	GRPCAuthFailuresTotal *prometheus.CounterVec
}

var (
	authnMetricsOnce sync.Once
	authnMetrics     *AuthnMetrics
)

// NewAuthnMetrics returns the process-wide authn metric set, registering the
// collectors on first call.
func NewAuthnMetrics() *AuthnMetrics {
	authnMetricsOnce.Do(func() {
		authnMetrics = &AuthnMetrics{
			TenantConflictsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
				Name: "OtelContext_auth_tenant_conflicts_total",
				Help: "Client-asserted tenants ignored because an authenticated key binds the request, by surface (http|ws|grpc) and carrier (header|query|metadata|resource_attribute).",
			}, []string{"surface", "reason"}),
			GRPCAuthFailuresTotal: promauto.NewCounterVec(prometheus.CounterOpts{
				Name: "OtelContext_grpc_auth_failures_total",
				Help: "gRPC authentication failures by reason (missing_header|bad_scheme|bad_key).",
			}, []string{"reason"}),
		}
	})
	return authnMetrics
}
